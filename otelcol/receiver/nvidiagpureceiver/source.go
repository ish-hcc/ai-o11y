package nvidiagpureceiver

import "context"

// Source reads one sample of every GPU.
type Source interface {
	// Namespace prefixes the source's metric names, such as "nvidia_smi".
	Namespace() string
	Read(ctx context.Context) ([]GPU, error)
}

// GPU is one GPU's sample.
type GPU struct {
	// Attributes identify the GPU, such as index, uuid and name.
	Attributes map[string]string
	// PCIBusID is the GPU's PCI address as the source writes it, such as
	// 00000001:00:00.0, empty when unknown.
	PCIBusID string
	Values   []Value
}

// Value is one field of a GPU's sample, under the source's own field name.
type Value struct {
	Field string
	// Unit is the UCUM unit, empty when dimensionless.
	Unit  string
	Value float64
}
