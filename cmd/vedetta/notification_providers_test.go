package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/notify"
)

func TestPushoverWorksWithoutWebPush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 30)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Notifications.Pushover = config.PushoverConfig{Enabled: true, AppTokenFile: path, UserKeyFile: path}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := setupNotificationProviders(ctx, cfg, nil)
	p, ok := provider.(*notify.Pushover)
	if !ok {
		t.Fatalf("Pushover must work without VAPID or browser subscriptions; got %T", provider)
	}
	cancel()
	p.Wait()
}

func TestNoNotificationProvidersIsNil(t *testing.T) {
	if got := setupNotificationProviders(context.Background(), config.Defaults(), nil); got != nil {
		t.Fatalf("no providers must be nil, got %T", got)
	}
}
