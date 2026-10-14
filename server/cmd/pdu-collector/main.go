// pdu-collector reads the hypervisors over SSH and pushes the readings to the PDU Monitor server. It is passive: no web interface, no
// login, no listening port.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/kblaskoff/pdu-monitor/server/internal/buildinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/collector"
	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
)

// version is the commit the binary was built from (the updater builds in a clone); -ldflags "-X main.version=1.0.0" overrides it
var version = "dev"

func init() {
	if version == "dev" {
		version = buildinfo.Version("dev")
	}
}

const usage = `pdu-collector - reads hypervisors and pushes the readings to the PDU Monitor server

  pdu-collector run   -config /etc/pdu-collector/config.yaml      read and push until stopped
  pdu-collector init  [-config /etc/pdu-collector/config.yaml]    write an example configuration file
  pdu-collector pubkey -config FILE                                the public key of this collector (for hostagent/install.sh --key)
  pdu-collector trust -config FILE -host NAME | -all               confirm the SSH key of a host (compare the fingerprint with the host)
  pdu-collector once  -config FILE                                 one reading and push, then stop (to try it)
  pdu-collector version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:], false)
	case "once":
		err = run(os.Args[2:], true)
	case "init":
		err = initConfig(os.Args[2:])
	case "pubkey":
		err = pubkey(os.Args[2:])
	case "trust":
		err = trust(os.Args[2:], os.Stdin)
	case "version", "-v", "--version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pdu-collector:", err)
		os.Exit(1)
	}
}

func configFlag(name string, args []string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	path := fs.String("config", "/etc/pdu-collector/config.yaml", "configuration file")
	return fs, path
}

func run(args []string, once bool) error {
	fs, path := configFlag("run", args)
	_ = fs.Parse(args)
	cfg, err := collector.Load(*path)
	if err != nil {
		return err
	}
	token, err := collector.ReadToken(cfg.TokenFile)
	if err != nil {
		return err
	}
	httpClient, err := collector.NewHTTPClient(cfg.CAFile)
	if err != nil {
		return err
	}
	signer, err := loadKey(cfg)
	if err != nil {
		return err
	}
	sshc := hostinfo.NewSSHClient(signer)
	defer sshc.Close()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	c := &collector.Collector{Cfg: cfg, Runner: sshc, Token: token, HTTP: httpClient, Version: version, Log: func(s string) { logger.Print(s) }}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Printf("pdu-collector %s: %d hosts, every %d s, to %s", version, len(cfg.Hosts), cfg.PollSeconds, cfg.ServerURL)
	if once {
		return c.Cycle(ctx)
	}
	c.Run(ctx)
	return nil
}

func loadKey(cfg *collector.Config) (ssh.Signer, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	return hostinfo.LoadOrCreateSSHKey(filepath.Join(cfg.DataDir, "ssh_key"))
}

func initConfig(args []string) error {
	fs, p := configFlag("init", args)
	_ = fs.Parse(args)
	if _, err := os.Stat(*p); err == nil {
		return errors.New(*p + " exists: it is not overwritten")
	}
	if err := os.MkdirAll(filepath.Dir(*p), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(*p, []byte(collector.Example), 0o640); err != nil {
		return err
	}
	fmt.Println("written:", *p)
	return nil
}

func pubkey(args []string) error {
	fs, path := configFlag("pubkey", args)
	_ = fs.Parse(args)
	cfg, err := collector.Load(*path)
	if err != nil {
		return err
	}
	signer, err := loadKey(cfg)
	if err != nil {
		return err
	}
	fmt.Println(hostinfo.PublicKeyLine(signer))
	return nil
}

// trust shows the key a host offers and stores it when the person types the fingerprint they saw on the host. Without that
// comparison somebody in the network could pretend to be the hypervisor.
func trust(args []string, in *os.File) error {
	fs, path := configFlag("trust", args)
	host := fs.String("host", "", "name of the host")
	all := fs.Bool("all", false, "every host whose key is not confirmed yet")
	_ = fs.Parse(args)
	if *host == "" && !*all {
		return errors.New("say which host: -host NAME, or -all")
	}
	cfg, err := collector.Load(*path)
	if err != nil {
		return err
	}
	signer, err := loadKey(cfg)
	if err != nil {
		return err
	}
	known, err := collector.LoadKnown(cfg.DataDir)
	if err != nil {
		return err
	}
	client := hostinfo.NewSSHClient(signer)
	defer client.Close()
	reader := bufio.NewReader(in)
	did := false
	for _, h := range cfg.Hosts {
		if (*host != "" && h.Name != *host) || (*host == "" && known.Key(h.Name, h.Address, h.Port) != "") {
			continue
		}
		did = true
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		key, err := client.FetchHostKey(ctx, h.Address, h.Port)
		cancel()
		if err != nil {
			fmt.Printf("%s: could not connect: %v\n", h.Name, err)
			continue
		}
		finger := ssh.FingerprintSHA256(key)
		fmt.Printf("\n%s (%s:%d) offers the key\n  %s\n", h.Name, h.Address, h.Port, finger)
		fmt.Printf("Compare it with the host:  ssh-keygen -lf /etc/ssh/ssh_host_%s_key.pub\n", keyFile(key.Type()))
		if old := known.Key(h.Name, h.Address, h.Port); old != "" {
			fmt.Println("WARNING: a different key was confirmed before. Continue only if the host was reinstalled.")
		}
		fmt.Print("Type the fingerprint from the host to confirm (empty = skip): ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			fmt.Println("skipped")
		case line != finger:
			fmt.Println("NOT the same: nothing was saved")
		default:
			if err := known.Confirm(cfg.DataDir, h.Name, h.Address, h.Port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))); err != nil {
				return err
			}
			fmt.Println("confirmed")
		}
	}
	if !did {
		fmt.Println("nothing to confirm")
	}
	return nil
}

func keyFile(t string) string {
	switch t {
	case "ssh-ed25519":
		return "ed25519"
	case "ssh-rsa":
		return "rsa"
	}
	return "ecdsa"
}
