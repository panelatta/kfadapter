package main

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// healthcheck asks the management endpoint's /healthz. It does not dial the
// SOCKS5 port, which the console can turn off; a failed SOCKS5 accept loop
// ends the daemon instead.
func healthcheck(arguments []string) error {
	cfg, err := loadConfig(arguments)
	if err != nil {
		return err
	}
	managementURL := "http://" + cfg.ManagementAddress() + "/healthz"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, managementURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("management endpoint not live")
	}
	return nil
}
