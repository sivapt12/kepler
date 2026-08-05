// SPDX-FileCopyrightText: 2025 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

//go:build goamdsmi

package esmi

// This file is only compiled when built with -tags goamdsmi.
// It requires:
//   - github.com/ROCm/amdsmi Go module (go get github.com/ROCm/amdsmi)
//   - libamd_smi.so installed (typically /opt/rocm/lib)
//   - amd_hsmp kernel driver loaded
//
// Zone types provided by this backend:
//   - socket : accumulated energy (µJ) + instantaneous power (W) per socket
//   - core   : accumulated energy (µJ) per logical thread/core

import (
	"fmt"
	"math"

	goamdsmi "github.com/ROCm/amdsmi"

	device "github.com/sustainable-computing-io/kepler/internal/device"
)

const (
	// Sentinel values returned by the goamdsmi library on failure.
	goamdsmiEnergyFailure = uint64(math.MaxUint64)
	goamdsmiPowerFailure  = uint64(math.MaxUint32)
)

// ── GoamdsmiReader ────────────────────────────────────────────────────────────

// GoamdsmiReader implements Reader using the AMD goamdsmi Go bindings.
// It returns one "socket" zone per physical CPU socket and one "core" zone
// per logical thread exposed by the library.
type GoamdsmiReader struct{}

// NewGoamdsmiReader returns a GoamdsmiReader.
func NewGoamdsmiReader() *GoamdsmiReader { return &GoamdsmiReader{} }

// Zones initialises the goamdsmi CPU subsystem and returns all available
// energy zones. Returns nil (no error) when the library cannot initialise so
// NewCPUPowerMeter falls through to the next backend.
func (r *GoamdsmiReader) Zones() ([]device.EnergyZone, error) {
	if !goamdsmi.GO_cpu_init() {
		return nil, nil
	}

	var zones []device.EnergyZone

	// ── socket zones ─────────────────────────────────────────────────────────
	numSockets := int(goamdsmi.GO_cpu_number_of_sockets_get())
	for i := 0; i < numSockets; i++ {
		if uint64(goamdsmi.GO_cpu_socket_energy_get(i)) == goamdsmiEnergyFailure {
			continue
		}
		zones = append(zones, &GoamdsmiPackageZone{socketIdx: i})
	}

	// ── core zones ────────────────────────────────────────────────────────────
	// The library indexes cores by logical thread (SMT thread index).
	numThreads := int(goamdsmi.GO_cpu_number_of_threads_get())
	threadsPerCore := int(goamdsmi.GO_cpu_threads_per_core_get())
	if threadsPerCore < 1 {
		threadsPerCore = 1
	}
	for i := 0; i < numThreads; i++ {
		if uint64(goamdsmi.GO_cpu_core_energy_get(i)) == goamdsmiEnergyFailure {
			continue
		}
		zones = append(zones, &GoamdsmiCoreZone{
			threadIdx: i,
			coreIdx:   i / threadsPerCore,
		})
	}

	return zones, nil
}

// ── GoamdsmiPackageZone ────────────────────────────────────────────────────────

// GoamdsmiPackageZone implements device.EnergyZone for one AMD CPU socket.
// Uniquely among ESMI zones, it also supports Power() for instantaneous watts.
type GoamdsmiPackageZone struct {
	socketIdx int
}

func (z *GoamdsmiPackageZone) Name() string { return "package" }
func (z *GoamdsmiPackageZone) Index() int   { return z.socketIdx }
func (z *GoamdsmiPackageZone) Path() string {
	return fmt.Sprintf("goamdsmi://cpu/socket/%d", z.socketIdx)
}

// Energy returns accumulated socket energy in microjoules.
func (z *GoamdsmiPackageZone) Energy() (device.Energy, error) {
	val := uint64(goamdsmi.GO_cpu_socket_energy_get(z.socketIdx))
	if val == goamdsmiEnergyFailure {
		return 0, fmt.Errorf("esmi/goamdsmi: GO_cpu_socket_energy_get failed for socket %d", z.socketIdx)
	}
	return device.Energy(val), nil
}

// MaxEnergy returns 0 — the library does not expose a counter maximum.
func (z *GoamdsmiPackageZone) MaxEnergy() device.Energy { return 0 }

// Power returns instantaneous socket power in watts.
// GO_cpu_socket_power_get returns milliwatts.
func (z *GoamdsmiPackageZone) Power() (device.Power, error) {
	val := uint64(goamdsmi.GO_cpu_socket_power_get(z.socketIdx))
	if val == goamdsmiPowerFailure {
		return 0, fmt.Errorf("esmi/goamdsmi: GO_cpu_socket_power_get failed for socket %d", z.socketIdx)
	}
	return device.Power(float64(val) / 1000.0), nil
}

// ── GoamdsmiCoreZone ──────────────────────────────────────────────────────────

// GoamdsmiCoreZone implements device.EnergyZone for a single logical core
// (SMT thread). Energy is cumulative µJ; instantaneous power is not available
// at per-core granularity via this library.
type GoamdsmiCoreZone struct {
	threadIdx int // library index (SMT thread)
	coreIdx   int // physical core index (threadIdx / threadsPerCore)
}

func (z *GoamdsmiCoreZone) Name() string { return "core" }
func (z *GoamdsmiCoreZone) Index() int   { return z.threadIdx }
func (z *GoamdsmiCoreZone) Path() string {
	return fmt.Sprintf("goamdsmi://cpu/core/%d", z.threadIdx)
}

// Energy returns accumulated core energy in microjoules.
func (z *GoamdsmiCoreZone) Energy() (device.Energy, error) {
	val := uint64(goamdsmi.GO_cpu_core_energy_get(z.threadIdx))
	if val == goamdsmiEnergyFailure {
		return 0, fmt.Errorf("esmi/goamdsmi: GO_cpu_core_energy_get failed for thread %d", z.threadIdx)
	}
	return device.Energy(val), nil
}

// MaxEnergy returns 0 — no counter maximum is exposed.
func (z *GoamdsmiCoreZone) MaxEnergy() device.Energy { return 0 }

// Power is not available at per-core granularity via goamdsmi.
// Use socket-level Power() or derive from successive Energy() readings.
func (z *GoamdsmiCoreZone) Power() (device.Power, error) {
	return 0, fmt.Errorf("esmi/goamdsmi: per-core instantaneous power is not available; derive from successive Energy() calls")
}

// ── backend registration ──────────────────────────────────────────────────────

func goamdsmiBackend() backendCandidate {
	return backendCandidate{name: "goamdsmi", reader: NewGoamdsmiReader()}
}
