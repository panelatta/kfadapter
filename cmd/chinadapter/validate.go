package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"

	"github.com/oshinop/chinadapter/internal/config"
	"github.com/oshinop/chinadapter/internal/store"
)

// loadConfig never writes: healthcheck and validate-config only inspect.
func loadConfig(arguments []string) (config.Config, error) {
	if len(arguments) != 0 {
		return config.Config{}, errors.New("configuration arguments are not supported")
	}
	return config.Load(configPath)
}

func validateConfig(arguments []string) error {
	_, err := loadConfig(arguments)
	return err
}

// validateState delegates secure file, integrity, schema, and aggregate
// validation to the SQLite state store without writing the file.
func validateState(arguments []string) error {
	flags := flag.NewFlagSet("validate-state", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("file", "", "")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || strings.TrimSpace(*path) == "" {
		return errors.New("invalid validate-state arguments")
	}
	return store.ValidateSQLiteFile(context.Background(), *path)
}
