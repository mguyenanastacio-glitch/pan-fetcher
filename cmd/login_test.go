package cmd

import (
	"testing"

	"github.com/bytedance/mockey"
	"github.com/mguyenanastacio-glitch/pan-fetcher/config"
	"github.com/mguyenanastacio-glitch/pan-fetcher/p115"
)

func TestInitAgentPrefersQrcodeOverCookies(t *testing.T) {
	originalQRLogin := qrLogin
	originalCookies := cookies
	originalAgent := pAgent
	t.Cleanup(func() {
		qrLogin = originalQRLogin
		cookies = originalCookies
		pAgent = originalAgent
	})

	qrLogin = true
	cookies = "CLI_ORIGINAL_COOKIE"

	loadCalls := 0
	newAgentCalls := 0
	qrcodeCalls := 0
	newCalls := 0

	patchLoad := mockey.Mock(config.LoadWithOptions).To(func(cliParams config.CLIParams, options config.LoadOptions) (*config.Config, *config.ConfigSource, error) {
		loadCalls++
		return &config.Config{
			Auth: config.AuthConfig{Cookies: "CONFIG_COOKIE"},
			Server: config.ServerConfig{Port: 8115},
			Database: config.DatabaseConfig{Path: "db.sqlite"},
			P115: config.P115Config{},
		}, &config.ConfigSource{}, nil
	}).Build()
	defer patchLoad.UnPatch()

	patchNewAgent := mockey.Mock(p115.NewAgent).To(func(string) (*p115.Agent, error) {
		newAgentCalls++
		return &p115.Agent{}, nil
	}).Build()
	defer patchNewAgent.UnPatch()

	patchQrcode := mockey.Mock(p115.NewAgentByQrcode).To(func() (*p115.Agent, error) {
		qrcodeCalls++
		return &p115.Agent{}, nil
	}).Build()
	defer patchQrcode.UnPatch()

	patchNew := mockey.Mock(p115.New).To(func() (*p115.Agent, error) {
		newCalls++
		return &p115.Agent{}, nil
	}).Build()
	defer patchNew.UnPatch()

	cfg := initAgent(nil)

	if loadCalls != 1 {
		t.Fatalf("expected config to be loaded once, got %d", loadCalls)
	}
	if qrcodeCalls != 1 {
		t.Fatalf("expected qrcode login to be used once, got %d", qrcodeCalls)
	}
	if newAgentCalls != 0 {
		t.Fatalf("expected cookie login to be skipped, got %d calls", newAgentCalls)
	}
	if newCalls != 0 {
		t.Fatalf("expected default login to be skipped, got %d calls", newCalls)
	}
	if cfg.Auth.Cookies != "CONFIG_COOKIE" {
		t.Fatalf("unexpected config returned: %+v", cfg.Auth)
	}
	if pAgent == nil {
		t.Fatal("expected pAgent to be initialized")
	}
}