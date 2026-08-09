// Package config loads sandboxd's configuration from the environment.
package config

import (
	"fmt"
	"os"
)

type Config struct {
	ListenAddr    string
	DatabaseURL   string
	NFSExportHost string // NFS server hostname, e.g. nfs-server - sandboxd dials it directly (see internal/storage), no local mount needed
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:    getEnv("LISTEN_ADDR", ":8080"),
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		NFSExportHost: getEnv("NFS_EXPORT_HOST", "localhost"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
