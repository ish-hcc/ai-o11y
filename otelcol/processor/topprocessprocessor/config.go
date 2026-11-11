package topprocessprocessor

import (
	"errors"
	"fmt"

	"go.opentelemetry.io/collector/component"
)

// Ranking criteria for Config.By.
const (
	// ByCPU ranks processes by the CPU time they used since the previous
	// batch.
	ByCPU = "cpu"
	// ByMemory ranks processes by their resident memory
	// (process.memory.usage).
	ByMemory = "memory"
)

// Config is the processor's configuration.
type Config struct {
	// TopN is how many processes each criterion keeps.
	TopN int `mapstructure:"top_n"`
	// By lists the criteria: cpu, memory or both. With both, the processes
	// either one keeps stay.
	By []string `mapstructure:"by"`
}

var _ component.Config = (*Config)(nil)

// Validate implements component.ConfigValidator.
func (c *Config) Validate() error {
	if c.TopN < 1 {
		return errors.New("top_n must be at least 1")
	}
	if len(c.By) == 0 {
		return errors.New("by is empty")
	}
	for _, b := range c.By {
		if b != ByCPU && b != ByMemory {
			return fmt.Errorf("unknown criterion %q in by (cpu or memory)", b)
		}
	}
	return nil
}
