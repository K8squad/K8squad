package config

import "time"

// Config provides the configuration for the backup DevOps system
type Config struct {
	// Debug enables debug logging
	Debug bool

	// Timeout is the default timeout for operations
	Timeout time.Duration
}
