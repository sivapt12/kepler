// SPDX-FileCopyrightText: 2025 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

//go:build goamdsmi

package esmi

// DimmReader provides per-DIMM power readings via the AMD E-SMI C library
// (libe_smi.so) through CGo. It is compiled only with -tags goamdsmi.
//
// The E-SMI library exposes esmi_get_dimm_power(socket, dimmAddr, *power)
// which returns instantaneous DIMM power in milliwatts. DIMM addresses are
// enumerated per-socket; the library uses the SPD address (0x50–0x57).
//
// Unlike the energy-counter backends, DimmReader zones only support Power()
// (instantaneous mW → W). Energy() returns an error directing callers to
// integrate Power() readings over time.
//
// Build requirements:
//   The E-SMI library (libe_smi64.so) and headers must be installed.
//   Default search paths: /usr/include, /usr/lib, /usr/local/include, /usr/local/lib
//   Override with environment variables:
//     CGO_CFLAGS="-I/path/to/esmi/include"
//     CGO_LDFLAGS="-L/path/to/esmi/lib"

/*
#cgo LDFLAGS: -le_smi64

#include <stdint.h>
#include <e_smi/e_smi.h>

// esmi_dimm_power_read is a thin C helper that calls
// esmi_dimm_power_consumption_get and returns the power in milliwatts,
// or UINT32_MAX on any error.
static uint32_t esmi_dimm_power_read(uint32_t socket, uint8_t dimm_addr) {
    struct dimm_power dp = {0};
    esmi_status_t ret = esmi_dimm_power_consumption_get(socket, dimm_addr, &dp);
    if (ret != ESMI_SUCCESS) {
        return UINT32_MAX;
    }
    return dp.power; // milliwatts
}

// esmi_init_safe wraps esmi_init and returns 0 on success.
static int esmi_init_safe(void) {
    return (int)esmi_init();
}

// esmi_num_sockets returns the number of sockets or 0 on error.
static uint32_t esmi_num_sockets(void) {
    uint32_t n = 0;
    esmi_number_of_sockets_get(&n);
    return n;
}
*/
import "C"

import (
	"fmt"

	device "github.com/sustainable-computing-io/kepler/internal/device"
)

// Standard SPD DIMM addresses on the SMBus: 0x80 through 0x8b
// (up to 8 DIMMs per socket channel).
var dimmAddrs = []uint8{0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89, 0x8a, 0x8b}

const dimmPowerFailure = uint32(0xFFFFFFFF)

// ── DimmReader ────────────────────────────────────────────────────────────────

// DimmReader enumerates all responding DIMM addresses across all sockets and
// returns one EsmiDimmZone per DIMM that responds to a power query.
type DimmReader struct{}

// NewDimmReader returns a DimmReader.
func NewDimmReader() *DimmReader { return &DimmReader{} }

// Zones initialises the e-smi library and probes all known DIMM SPD addresses
// on every socket. Only DIMMs that return a valid power reading are included.
func (r *DimmReader) Zones() ([]device.EnergyZone, error) {
	if rc := C.esmi_init_safe(); rc != 0 {
		// Library or driver not available — return nil so the caller can
		// skip this reader without treating it as a hard error.
		return nil, nil
	}

	numSockets := int(C.esmi_num_sockets())
	if numSockets == 0 {
		return nil, nil
	}

	var zones []device.EnergyZone
	for socket := 0; socket < numSockets; socket++ {
		for i, addr := range dimmAddrs {
			pw := uint32(C.esmi_dimm_power_read(C.uint32_t(socket), C.uint8_t(addr)))
			if pw == dimmPowerFailure {
				continue // no DIMM at this address
			}
			zones = append(zones, &EsmiDimmZone{
				socketIdx: socket,
				dimmAddr:  addr,
				// Index encodes socket and slot: socket*8 + slot
				idx: socket*len(dimmAddrs) + i,
			})
		}
	}
	return zones, nil
}

// ── EsmiDimmZone ──────────────────────────────────────────────────────────────

// EsmiDimmZone implements device.EnergyZone for a single DIMM.
// Only Power() is supported; Energy() returns an error.
type EsmiDimmZone struct {
	socketIdx int
	dimmAddr  uint8
	idx       int
}

func (z *EsmiDimmZone) Name() string { return "dimm" }
func (z *EsmiDimmZone) Index() int   { return z.idx }
func (z *EsmiDimmZone) Path() string {
	return fmt.Sprintf("esmi://cpu/socket/%d/dimm/0x%02x", z.socketIdx, z.dimmAddr)
}

// Energy is not supported for DIMM zones. The E-SMI library exposes only
// instantaneous power; integrate Power() readings over time to get energy.
func (z *EsmiDimmZone) Energy() (device.Energy, error) {
	return 0, fmt.Errorf("esmi/dimm: DIMM zones do not provide energy counters; integrate Power() readings over time")
}

// MaxEnergy returns 0 (not applicable).
func (z *EsmiDimmZone) MaxEnergy() device.Energy { return 0 }

// Power returns the instantaneous DIMM power in watts.
// The E-SMI library returns milliwatts.
func (z *EsmiDimmZone) Power() (device.Power, error) {
	pw := uint32(C.esmi_dimm_power_read(C.uint32_t(z.socketIdx), C.uint8_t(z.dimmAddr)))
	if pw == dimmPowerFailure {
		return 0, fmt.Errorf("esmi/dimm: esmi_get_dimm_power failed for socket %d dimm 0x%02x",
			z.socketIdx, z.dimmAddr)
	}
	return device.Power(float64(pw) / 1000.0), nil
}

// ── backend registration ──────────────────────────────────────────────────────

// dimmBackend returns a backend candidate for DIMM power readings.
// It is included in availableReaders() alongside the goamdsmi backend.
func dimmBackend() backendCandidate {
	return backendCandidate{name: "dimm", reader: NewDimmReader()}
}
