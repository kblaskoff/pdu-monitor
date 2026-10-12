// pdumonitord is the PDU Monitor server: the web interface, the collectors of the PDUs and the hypervisors, and the login.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
	"github.com/kblaskoff/pdu-monitor/server/internal/config"
	"github.com/kblaskoff/pdu-monitor/server/internal/demo"
	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
	"github.com/kblaskoff/pdu-monitor/server/internal/web"
)

// version is set when building: -ldflags "-X main.version=1.0.0"
var version = "dev"

const usage = `pdumonitord - PDU Monitor server

  pdumonitord serve -config /etc/pdu-monitor/config.yaml     run the server
  pdumonitord serve -demo [-listen 127.0.0.1:8080]           run with sample data, nothing is sent to any device (login: demo / demo)
  pdumonitord init [-config /etc/pdu-monitor/config.yaml]    write an example configuration file
  pdumonitord ldap-test -config FILE -user NAME              try a login against the directory and show every step
  pdumonitord pubkey -config FILE                            the public key of this server (for install.sh --key of the host agent)
  pdumonitord collector-token -name NAME                     make the token of a collector (for pdu-collector) and the lines for this configuration
  pdumonitord hash-password                                  make the hash for local_admin.password_hash
  pdumonitord selfsigned -host NAME -out DIR                 make a self-signed certificate (for a first start or a lab)
  pdumonitord version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "init":
		err = initConfig(os.Args[2:])
	case "ldap-test":
		err = ldapTest(os.Args[2:])
	case "pubkey":
		err = pubkey(os.Args[2:])
	case "collector-token":
		err = collectorToken(os.Args[2:])
	case "hash-password":
		err = hashPassword()
	case "selfsigned":
		err = selfSigned(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---- serve

type demoAuth struct{}

func (demoAuth) Authenticate(_ context.Context, user, pass string) (*auth.User, error) {
	roles := map[string]auth.Role{"demo": auth.RoleAdmin, "operator": auth.RoleOperator, "viewer": auth.RoleViewer}
	if role, ok := roles[user]; ok && pass == user {
		return &auth.User{Name: user, DisplayName: strings.ToUpper(user[:1]) + user[1:], Role: role, Source: "demo"}, nil
	}
	return nil, auth.ErrBadCredentials
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/pdu-monitor/config.yaml", "configuration file")
	demoMode := fs.Bool("demo", false, "sample data, no real devices, no LDAP")
	listen := fs.String("listen", "", "address for -demo (default 127.0.0.1:8080)")
	_ = fs.Parse(args)

	logger := log.New(os.Stdout, "", log.LstdFlags)
	var cfg *config.Config
	if *demoMode {
		dir, err := os.MkdirTemp("", "pdu-monitor-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		cfg = &config.Config{Listen: "127.0.0.1:8080", DataDir: dir, AllowPlainHTTP: true}
		if *listen != "" {
			cfg.Listen = *listen
		}
		cfg.Session.IdleMinutes, cfg.Session.MaxHours = 480, 24
	} else {
		var err error
		if cfg, err = config.Load(*cfgPath); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	debug := func(string) {}
	if cfg.Debug {
		debug = func(s string) { logger.Print("debug: ", s) }
	}

	key, err := store.LoadOrCreateKey(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return err
	}
	secrets, err := store.NewSecrets(key)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "pdu-monitor.db"), secrets)
	if err != nil {
		return err
	}
	defer st.Close()
	signer, err := hostinfo.LoadOrCreateSSHKey(filepath.Join(cfg.DataDir, "ssh_ed25519"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		authn   auth.Authenticator
		factory app.DriverFactory
		runner  hostinfo.Runner
		keys    web.KeyFetcher
		ssh     *hostinfo.SSHClient
	)
	if *demoMode {
		lab, err := demo.Seed(ctx, st)
		if err != nil {
			return err
		}
		authn, factory, runner = demoAuth{}, lab.Factory, lab.Runner()
		keys = demo.Keys{}
		logger.Print("DEMO MODE: sample data only; log in as demo / demo (admin), operator / operator, viewer / viewer")
	} else {
		ldapCfg, err := cfg.LDAPConfig()
		if err != nil {
			return err
		}
		ldapCfg.Debug = debug
		dir, err := auth.NewDirectory(ldapCfg)
		if err != nil {
			return err
		}
		chain := &auth.Chain{LDAP: dir}
		if cfg.LocalAdmin.PasswordHash != "" {
			chain.Local = &auth.LocalAdmin{Username: cfg.LocalAdmin.Username, Hash: cfg.LocalAdmin.PasswordHash}
			logger.Print("the emergency account is enabled")
		}
		authn = chain
		factory = app.SNMPDriver(debug)
		ssh = hostinfo.NewSSHClient(signer)
		defer ssh.Close()
		runner, keys = ssh, ssh
	}

	poller := hostinfo.NewPoller(st, runner)
	poller.Log = func(s string) { logger.Print(s) }
	application := app.New(st, poller, factory)
	application.Log = func(s string) { logger.Print(s) }
	sessions := auth.NewSessions(time.Duration(cfg.Session.IdleMinutes)*time.Minute, time.Duration(cfg.Session.MaxHours)*time.Hour)
	srv := web.New(application, st, authn, sessions)
	srv.Keys = keys
	srv.ServerKey = hostinfo.PublicKeyLine(signer)
	srv.Secure = cfg.TLSCert != ""
	for _, c := range cfg.Collectors {
		hash, _ := hex.DecodeString(c.TokenHash) // checked when the file was read
		ca := web.CollectorAuth{Name: c.Name}
		copy(ca.Hash[:], hash)
		srv.Collectors = append(srv.Collectors, ca)
	}
	srv.Demo = *demoMode
	srv.Version = version
	srv.Log = func(s string) { logger.Print(s) }

	go application.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Minute):
				sessions.Reap()
			}
		}
	}()

	httpServer := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
		WriteTimeout: 120 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: logger}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	if cfg.TLSCert != "" {
		reloader := &certReloader{cert: cfg.TLSCert, key: cfg.TLSKey}
		if _, err := reloader.get(nil); err != nil {
			return fmt.Errorf("TLS certificate: %w", err)
		}
		httpServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: reloader.get}
		listener = tls.NewListener(listener, httpServer.TLSConfig)
		logger.Printf("pdumonitord %s listening on https://%s", version, cfg.Listen)
	} else {
		logger.Printf("pdumonitord %s listening on http://%s (no TLS)", version, cfg.Listen)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Print("stopped")
	return nil
}

// certReloader reads the certificate again when the file changes (a renewal needs no restart).
type certReloader struct {
	cert, key string
	mu        sync.Mutex
	loaded    time.Time
	pair      *tls.Certificate
	checked   time.Time
}

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pair != nil && time.Since(c.checked) < time.Minute {
		return c.pair, nil
	}
	c.checked = time.Now()
	st, err := os.Stat(c.cert)
	if err != nil {
		if c.pair != nil {
			return c.pair, nil
		}
		return nil, err
	}
	if c.pair == nil || st.ModTime().After(c.loaded) {
		pair, err := tls.LoadX509KeyPair(c.cert, c.key)
		if err != nil {
			if c.pair != nil {
				return c.pair, nil // keep the old one rather than stop answering
			}
			return nil, err
		}
		c.pair, c.loaded = &pair, st.ModTime()
	}
	return c.pair, nil
}

// ---- helper commands

func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", "/etc/pdu-monitor/config.yaml", "where to write it")
	_ = fs.Parse(args)
	if _, err := os.Stat(*path); err == nil {
		return fmt.Errorf("%s exists already: not overwritten", *path)
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(*path, []byte(config.Example), 0o640); err != nil {
		return err
	}
	fmt.Println("written:", *path, "- edit it, then: pdumonitord ldap-test -config", *path, "-user YOUR_NAME")
	return nil
}

func ldapTest(args []string) error {
	fs := flag.NewFlagSet("ldap-test", flag.ExitOnError)
	path := fs.String("config", "/etc/pdu-monitor/config.yaml", "configuration file")
	user := fs.String("user", "", "user name to try")
	_ = fs.Parse(args)
	if *user == "" {
		return errors.New("-user is needed")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ldapCfg, err := cfg.LDAPConfig()
	if err != nil {
		return err
	}
	ldapCfg.Debug = func(s string) { fmt.Println("  ·", s) }
	dir, err := auth.NewDirectory(ldapCfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Password for %s: ", *user)
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	u, err := dir.Authenticate(context.Background(), *user, string(pw))
	if err != nil {
		fmt.Println("RESULT: refused:", err)
		return nil
	}
	fmt.Printf("RESULT: ok. %s (%s) gets the role %q through the groups %v\n", u.Name, u.DisplayName, u.Role, u.Groups)
	return nil
}

func pubkey(args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
	path := fs.String("config", "/etc/pdu-monitor/config.yaml", "configuration file")
	_ = fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	signer, err := hostinfo.LoadOrCreateSSHKey(filepath.Join(cfg.DataDir, "ssh_ed25519"))
	if err != nil {
		return err
	}
	fmt.Println(hostinfo.PublicKeyLine(signer))
	return nil
}

// collectorToken makes a random token. The collector keeps the token (in a file only it can read); this server keeps only its SHA-256.
func collectorToken(args []string) error {
	fs := flag.NewFlagSet("collector-token", flag.ExitOnError)
	name := fs.String("name", "", "name of the collector, e.g. collector-a")
	_ = fs.Parse(args)
	if *name == "" || strings.ContainsAny(*name, " \t") {
		return errors.New("give the name of the collector: -name collector-a")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	fmt.Printf("Token (put it in the token file of the collector, mode 600; it is not kept here and cannot be shown again):\n\n  %s\n\n", token)
	fmt.Printf("Add this to the configuration of this server and restart it:\n\ncollectors:\n  - name: %s\n    token_sha256: \"%s\"\n", *name, hex.EncodeToString(sum[:]))
	return nil
}

func hashPassword() error {
	fmt.Fprint(os.Stderr, "Password: ")
	var pw []byte
	var err error
	if term.IsTerminal(int(os.Stdin.Fd())) {
		pw, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
	} else {
		line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
		pw, err = []byte(strings.TrimRight(line, "\r\n")), rerr
		if errors.Is(err, os.ErrClosed) {
			err = nil
		}
	}
	if len(pw) < 12 {
		return errors.New("use at least 12 characters")
	}
	hash, herr := bcrypt.GenerateFromPassword(pw, 12)
	if herr != nil {
		return herr
	}
	fmt.Println(string(hash))
	return nil
}

func selfSigned(args []string) error {
	fs := flag.NewFlagSet("selfsigned", flag.ExitOnError)
	host := fs.String("host", "", "the name (and/or IP) of this server")
	out := fs.String("out", ".", "folder for tls.crt and tls.key")
	_ = fs.Parse(args)
	if *host == "" {
		return errors.New("-host is needed")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: *host, Organization: []string{"PDU Monitor"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(2, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	for _, h := range strings.Split(*host, ",") {
		h = strings.TrimSpace(h)
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	fmt.Println("written", filepath.Join(*out, "tls.crt"), "and tls.key (valid 2 years). Browsers will warn about it: for real use take a certificate of your internal CA.")
	return nil
}
