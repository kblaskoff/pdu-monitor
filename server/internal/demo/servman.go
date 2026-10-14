package demo

import (
	"context"
	"fmt"

	"github.com/kblaskoff/pdu-monitor/server/internal/servman"
)

// demoServman is a servman with the sample servers: every server of every rack on both PDUs, plus a few differences, so that the screen has
// something to show: new outlets to name, one that is renamed, a conflict and a PDU that does not exist here.
type demoServman struct{}

// Servman is the sample inventory.
func (l *Lab) Servman() servman.Source { return demoServman{} }

func (demoServman) Devices(context.Context) ([]servman.Device, error) {
	var out []servman.Device
	id := int64(1)
	for _, spec := range racks {
		for n, s := range spec.servers {
			ccid := s[0].(string)
			if ccid == "200U03" { // servman does not know this one: its outlets are only reported, never touched
				continue
			}
			ports := []servman.Port{{PDU: "P" + spec.name + "A", Port: n + 1}}
			if n+1 <= 16 {
				ports = append(ports, servman.Port{PDU: "P" + spec.name + "B", Port: n + 1})
			}
			out = append(out, servman.Device{ID: id, CCID: ccid, Name: fmt.Sprintf("host%d", id), Status: "active", Ports: ports})
			id++
		}
	}
	extra := []servman.Device{
		{CCID: "100U50", Name: "web50", Ports: []servman.Port{{PDU: "P100A", Port: 20}, {PDU: "P100B", Port: 14}}}, // new: the outlets still have the default names
		{CCID: "200U33", Name: "db33", Ports: []servman.Port{{PDU: "P200A", Port: 13}}},                            // renames an outlet that has another name (200U03)
		{CCID: "10FU40", Name: "a", Ports: []servman.Port{{PDU: "P10FA", Port: 20}}},                               // two devices on one outlet: a conflict
		{CCID: "10FU41", Name: "b", Ports: []servman.Port{{PDU: "P10FA", Port: 20}}},
		{CCID: "10FU42", Name: "c", Ports: []servman.Port{{PDU: "P10FC", Port: 1}}}, // a PDU that is not in this system
	}
	for _, d := range extra {
		d.ID, d.Status = id, "active"
		out = append(out, d)
		id++
	}
	return out, nil
}
