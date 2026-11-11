package amdgpureceiver

import (
	"errors"
	"time"

	"go.opentelemetry.io/collector/scraper/scraperhelper"
)

// SourceAMDSMI is the source that runs amd-smi.
const SourceAMDSMI = "amd_smi"

// Config is the receiver's configuration.
type Config struct {
	scraperhelper.ControllerConfig `mapstructure:",squash"`
	// Source picks where samples come from; only "amd_smi" exists yet.
	Source string       `mapstructure:"source"`
	AMDSMI AMDSMIConfig `mapstructure:"amd_smi"`
}

// AMDSMIConfig configures the amd-smi source.
type AMDSMIConfig struct {
	// Path is the amd-smi executable. A bare name not found on PATH is also
	// looked for where ROCm installs it (/opt/rocm/bin), since a service's PATH
	// rarely holds that directory.
	Path string `mapstructure:"path"`
}

// Validate implements component.ConfigValidator.
func (c *Config) Validate() error {
	if c.Source != SourceAMDSMI {
		return errors.New(`source must be "amd_smi"`)
	}
	if c.AMDSMI.Path == "" {
		return errors.New("amd_smi.path must not be empty")
	}
	if c.CollectionInterval <= 0 {
		return errors.New("collection_interval must be positive")
	}
	return nil
}

func createDefaultConfig() *Config {
	cc := scraperhelper.NewDefaultControllerConfig()
	cc.CollectionInterval = 30 * time.Second
	// amd-smi is a Python program that takes seconds per run, and the first
	// scrape runs it three times.
	cc.Timeout = 20 * time.Second
	return &Config{
		ControllerConfig: cc,
		Source:           SourceAMDSMI,
		AMDSMI:           AMDSMIConfig{Path: "amd-smi"},
	}
}
