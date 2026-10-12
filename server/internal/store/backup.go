package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
)

// The settings file of the Mac application (Settings → General → Export), format "pdu-monitor-settings" version 1.
type macBackup struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	Racks   []struct {
		ID      string  `json:"id"`
		Name    string  `json:"name"`
		MaxAmps float64 `json:"maxAmps"`
	} `json:"racks"`
	Devices []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		RackID      string   `json:"rackID"`
		Vendor      string   `json:"vendor"`
		Host        string   `json:"host"`
		Port        int      `json:"port"`
		MaxAmps     float64  `json:"maxAmps"`
		BankMaxAmps *float64 `json:"bankMaxAmps"`
	} `json:"devices"`
	Labels struct {
		Labels map[string]map[string]string `json:"labels"`
	} `json:"labels"`
	Settings struct {
		PollInterval float64 `json:"pollInterval"`
		RestartDelay float64 `json:"restartDelay"`
		WarnPercent  float64 `json:"warnPercent"`
	} `json:"settings"`
	Communities map[string]struct {
		Read  string `json:"read"`
		Write string `json:"write"`
	} `json:"communities"`
}

// ImportSummary says what an import did.
type ImportSummary struct {
	Racks            int      `json:"racks"`
	PDUs             int      `json:"pdus"`
	Labels           int      `json:"labels"`
	Skipped          []string `json:"skipped"`
	WithoutCommunity []string `json:"withoutCommunity"`
}

// ImportMacBackup adds the racks, PDUs and server names of a settings file made by the Mac application. Racks and PDUs whose name
// already exists are not touched. The communities are only in the file when it was exported with them; a PDU without one is added and
// must get its community in the editor before it can be read.
func (s *Store) ImportMacBackup(ctx context.Context, data []byte) (ImportSummary, error) {
	var sum ImportSummary
	var b macBackup
	if err := json.Unmarshal(data, &b); err != nil || b.Format != "pdu-monitor-settings" {
		return sum, errors.New("this file is not a PDU Monitor settings file")
	}
	if b.Version > 1 {
		return sum, errors.New("this file was made by a newer version")
	}
	racks, err := s.Racks(ctx)
	if err != nil {
		return sum, err
	}
	byName := map[string]int64{}
	for _, r := range racks {
		byName[strings.ToLower(r.Name)] = r.ID
	}
	rackIDs := map[string]int64{} // the uuid of the file -> id here
	for _, r := range b.Racks {
		if id, ok := byName[strings.ToLower(strings.TrimSpace(r.Name))]; ok {
			rackIDs[r.ID] = id
			sum.Skipped = append(sum.Skipped, "rack "+r.Name+" (exists)")
			continue
		}
		if r.MaxAmps <= 0 {
			r.MaxAmps = 24
		}
		saved, err := s.SaveRack(ctx, Rack{Name: r.Name, MaxAmps: r.MaxAmps})
		if err != nil {
			return sum, fmt.Errorf("rack %q: %w", r.Name, err)
		}
		rackIDs[r.ID] = saved.ID
		sum.Racks++
	}
	existing, err := s.PDUs(ctx)
	if err != nil {
		return sum, err
	}
	pduNames := map[string]bool{}
	for _, p := range existing {
		pduNames[strings.ToLower(p.Name)] = true
	}
	for _, d := range b.Devices {
		rack, ok := rackIDs[d.RackID]
		if !ok {
			sum.Skipped = append(sum.Skipped, "PDU "+d.Name+" (its rack is not in the file)")
			continue
		}
		if pduNames[strings.ToLower(strings.TrimSpace(d.Name))] {
			sum.Skipped = append(sum.Skipped, "PDU "+d.Name+" (exists)")
			continue
		}
		vendor := pdu.Vendor(strings.ToLower(d.Vendor))
		if d.Port == 0 {
			d.Port = 161
		}
		if d.MaxAmps <= 0 {
			d.MaxAmps = 24
		}
		c := Communities{}
		if given, ok := b.Communities[d.ID]; ok {
			c = Communities{Read: given.Read, Write: given.Write}
		}
		if _, err := s.SavePDU(ctx, PDU{RackID: rack, Name: d.Name, Vendor: vendor, Host: d.Host, Port: d.Port, MaxAmps: d.MaxAmps, BankMaxAmps: d.BankMaxAmps}, c, false); err != nil {
			sum.Skipped = append(sum.Skipped, fmt.Sprintf("PDU %s (%v)", d.Name, err))
			continue
		}
		sum.PDUs++
		if c.Read == "" {
			sum.WithoutCommunity = append(sum.WithoutCommunity, d.Name)
		}
	}
	for rackUUID, labels := range b.Labels.Labels {
		rack, ok := rackIDs[rackUUID]
		if !ok {
			continue
		}
		for key, label := range labels {
			if err := s.SetLabel(ctx, rack, key, label); err == nil {
				sum.Labels++
			}
		}
	}
	st, _ := s.Settings(ctx)
	if b.Settings.PollInterval > 0 {
		st.PollSeconds = int(b.Settings.PollInterval)
	}
	if b.Settings.RestartDelay > 0 {
		st.RestartDelaySecond = int(b.Settings.RestartDelay)
	}
	if b.Settings.WarnPercent > 0 {
		st.WarnPercent = int(b.Settings.WarnPercent)
	}
	if _, err := s.SaveSettings(ctx, st); err != nil {
		return sum, err
	}
	return sum, nil
}
