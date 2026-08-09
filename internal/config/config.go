package config

import (
	"fmt"
	"os"
)

type Config struct {
	ListenAddr string
	DBURL      string
	NFSHost    string
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr: getEnv("LISTEN_ADDR", ":8080"),
		DBURL:      os.Getenv("DB_URL"),
		NFSHost:    os.Getenv("NFS_HOST"),
	}
	if cfg.DBURL == "" {
		return Config{}, fmt.Errorf("DB_URL is required")
	}
	if cfg.NFSHost == "" {
		return Config{}, fmt.Errorf("NFS_HOST is required")
	}
	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
