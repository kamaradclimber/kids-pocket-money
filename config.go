package main

import (
	"fmt"
	"os"
	"strconv"
)

type config struct {
	ListenAddr   string
	DataDir      string
	AdminGroup   string
	KidsGroup    string
	SecureCookie bool
}

func loadConfig() (config, error) {
	secureCookie, err := boolEnv("SECURE_COOKIES", true)
	if err != nil {
		return config{}, err
	}

	cfg := config{
		ListenAddr:   env("LISTEN_ADDR", ":8080"),
		DataDir:      env("DATA_DIR", "/data"),
		AdminGroup:   env("ADMIN_GROUP", "parents"),
		KidsGroup:    env("KIDS_GROUP", "kids"),
		SecureCookie: secureCookie,
	}

	if cfg.ListenAddr == "" || cfg.DataDir == "" {
		return config{}, fmt.Errorf("LISTEN_ADDR and DATA_DIR must not be empty")
	}
	if cfg.AdminGroup == "" || cfg.KidsGroup == "" {
		return config{}, fmt.Errorf("ADMIN_GROUP and KIDS_GROUP must not be empty")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func boolEnv(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}
