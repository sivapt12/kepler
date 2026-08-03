// SPDX-FileCopyrightText: 2025 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package esmi

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	device "github.com/sustainable-computing-io/kepler/internal/device"
)

// ── SysfsReader ───────────────────────────────────────────────────────────────

// SysfsReader reads AMD energy zones from the amd_energy platform driver.
//
// The amd_energy driver (CONFIG_AMD_ENERGY, Linux ≥ 5.8) exposes hwmon-style
// attributes under:
//
//	/sys/bus/platform/drivers/amd_energy/amd_energy.<N>/hwmon/hwmon<M>/
//	    energy<K>_input   – accumulated energy in µJ (uint64, wraps at max)
//	    energy<K>_label   – human-readable label, e.g. "socket0", "core3"
//
// A fallback via /sys/class/hwmon is tried when the platform driver path
// contains no devices (some distributions symlink the nodes differently).
type SysfsReader struct {
	sysfsPath string
}

// NewSysfsReader returns a SysfsReader rooted at sysfsPath (normally "/sys").
func NewSysfsReader(sysfsPath string) *SysfsReader {
	return &SysfsReader{sysfsPath: sysfsPath}
}

func (r *SysfsReader) driverRoot() string {
	return filepath.Join(r.sysfsPath, "bus", "platform", "drivers", "amd_energy")
}

// Zones discovers all energy zones exposed by the amd_energy driver.
func (r *SysfsReader) Zones() ([]device.EnergyZone, error) {
	deviceDirs, err := filepath.Glob(filepath.Join(r.driverRoot(), "amd_energy.*"))
	if err != nil {
		return nil, fmt.Errorf("esmi/sysfs: glob driver root: %w", err)
	}
	if len(deviceDirs) == 0 {
		return r.zonesFromHwmonClass()
	}

	var zones []device.EnergyZone
	for _, dev := range deviceDirs {
		zs, err := r.zonesFromDevice(dev)
		if err != nil {
			slog.Default().Warn("esmi/sysfs: skipping device", "path", dev, "err", err)
			continue
		}
		zones = append(zones, zs...)
	}
	return zones, nil
}

func (r *SysfsReader) zonesFromDevice(deviceDir string) ([]device.EnergyZone, error) {
	hwmonDirs, err := filepath.Glob(filepath.Join(deviceDir, "hwmon", "hwmon*"))
	if err != nil || len(hwmonDirs) == 0 {
		return nil, fmt.Errorf("esmi/sysfs: no hwmon dir under %s", deviceDir)
	}
	var zones []device.EnergyZone
	for _, hwmon := range hwmonDirs {
		zs, err := r.zonesFromHwmonDir(hwmon)
		if err != nil {
			return nil, err
		}
		zones = append(zones, zs...)
	}
	return zones, nil
}

func (r *SysfsReader) zonesFromHwmonClass() ([]device.EnergyZone, error) {
	hwmonClass := filepath.Join(r.sysfsPath, "class", "hwmon")
	entries, err := os.ReadDir(hwmonClass)
	if err != nil {
		return nil, fmt.Errorf("esmi/sysfs: cannot read %s: %w", hwmonClass, err)
	}
	var zones []device.EnergyZone
	for _, entry := range entries {
		hwmonDir := filepath.Join(hwmonClass, entry.Name())
		nameBytes, err := os.ReadFile(filepath.Join(hwmonDir, "name"))
		if err != nil || strings.TrimSpace(string(nameBytes)) != "amd_energy" {
			continue
		}
		zs, err := r.zonesFromHwmonDir(hwmonDir)
		if err != nil {
			slog.Default().Warn("esmi/sysfs: skipping hwmon dir", "path", hwmonDir, "err", err)
			continue
		}
		zones = append(zones, zs...)
	}
	return zones, nil
}

func (r *SysfsReader) zonesFromHwmonDir(hwmonDir string) ([]device.EnergyZone, error) {
	inputs, err := filepath.Glob(filepath.Join(hwmonDir, "energy*_input"))
	if err != nil {
		return nil, err
	}
	zones := make([]device.EnergyZone, 0, len(inputs))
	for _, inputPath := range inputs {
		base := filepath.Base(inputPath)                // "energy3_input"
		trimmed := strings.TrimPrefix(base, "energy")   // "3_input"
		trimmed = strings.TrimSuffix(trimmed, "_input") // "3"
		idx, err := strconv.Atoi(trimmed)
		if err != nil {
			continue
		}
		labelPath := filepath.Join(hwmonDir, fmt.Sprintf("energy%d_label", idx))
		label := r.readLabel(labelPath, idx)

		maxPath := filepath.Join(hwmonDir, fmt.Sprintf("energy%d_max", idx))
		maxVal := r.readUint64OrZero(maxPath)

		zones = append(zones, &SysfsEnergyZone{
			name:      ZoneLabelToName(label),
			index:     idx,
			path:      inputPath,
			maxEnergy: device.Energy(maxVal),
		})
	}
	return zones, nil
}

func (r *SysfsReader) readLabel(path string, fallbackIdx int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("unknown%d", fallbackIdx)
	}
	return strings.TrimSpace(string(b))
}

func (r *SysfsReader) readUint64OrZero(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v
}

// ZoneLabelToName strips trailing digits from an AMD hwmon label and
// normalizes zone names to match RAPL naming conventions.
//
//	"socket0"  → "package" (normalized from "socket" to match RAPL)
//	"core23"   → "core"
//	"l3cache1" → "l3cache"
func ZoneLabelToName(label string) string {
	label = strings.ToLower(strings.TrimSpace(label))
	i := len(label)
	for i > 0 && label[i-1] >= '0' && label[i-1] <= '9' {
		i--
	}
	if i == 0 {
		return label
	}
	baseName := label[:i]

	// Normalize AMD zone names to match RAPL conventions
	if baseName == "socket" {
		return "package"
	}
	return baseName
}

// ── SysfsEnergyZone ───────────────────────────────────────────────────────────

// SysfsEnergyZone implements device.EnergyZone for a single amd_energy counter.
type SysfsEnergyZone struct {
	name      string
	index     int
	path      string
	maxEnergy device.Energy
}

func (z *SysfsEnergyZone) Name() string { return z.name }
func (z *SysfsEnergyZone) Index() int   { return z.index }
func (z *SysfsEnergyZone) Path() string { return z.path }

// Energy reads the accumulated energy counter in microjoules.
func (z *SysfsEnergyZone) Energy() (device.Energy, error) {
	b, err := os.ReadFile(z.path)
	if err != nil {
		return 0, fmt.Errorf("esmi/sysfs: read %s: %w", z.path, err)
	}
	val, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("esmi/sysfs: parse energy from %s: %w", z.path, err)
	}
	return device.Energy(val), nil
}

// MaxEnergy returns the counter rollover value; 0 means unknown.
func (z *SysfsEnergyZone) MaxEnergy() device.Energy { return z.maxEnergy }

// Power is unsupported; derive from successive Energy() readings.
func (z *SysfsEnergyZone) Power() (device.Power, error) {
	return 0, fmt.Errorf("esmi/sysfs: amd_energy zones do not provide instantaneous power; derive from successive Energy() calls")
}

// ── backend registration ──────────────────────────────────────────────────────

// sysfsBackend is called by availableReaders() in backends_*.go.
func sysfsBackend(sysfsPath string) backendCandidate {
	return backendCandidate{name: "sysfs", reader: NewSysfsReader(sysfsPath)}
}
