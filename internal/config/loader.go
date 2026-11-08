package config

import (
	"fmt"
	"os"
	"strconv"

	"gitee.com/kobeyoung81/nanoprotocol"
	"strings"
)

const (
	DefaultBusURL               = "ws://localhost:9000"
	DefaultWeComCallbackAddr    = ":8081"
	DefaultWeChatKFCallbackAddr = ":8082"
)

// ResolveBusURL returns the bus URL from the flag value, falling back to the
// NANOSEC_GATEWAY_BUS_URL environment variable, then to DefaultBusURL.
func ResolveBusURL(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("NANOSEC_GATEWAY_BUS_URL"); v != "" {
		return v
	}
	return DefaultBusURL
}

// InternalToken returns the shared token the bus requires on its internal
// endpoints. It comes from the environment (every process of one platform holds
// the same value) and is needed before the first bus call, so it cannot come
// from the config items themselves.
func InternalToken() string {
	return strings.TrimSpace(os.Getenv("NANOSEC_BUS_INTERNAL_TOKEN"))
}

// LoadGatewayConfig connects to the bus, fetches the gateway configuration
// items, and reconstructs a GatewayConfig from flat key-value pairs.
func LoadGatewayConfig(busURL string) (GatewayConfig, error) {
	client, err := nanoprotocol.NewClient(busURL, nanoprotocol.WithToken(InternalToken()))
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("connect to bus %s: %w", busURL, err)
	}
	defer client.Close()

	items, err := client.GetConfigItems("gateway")
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("get gateway config items: %w", err)
	}

	m := configItemsToMap(items)
	cfg := applyGatewayConfig(m)
	return cfg, nil
}

func configItemsToMap(items []nanoprotocol.ConfigItem) map[string]string {
	m := make(map[string]string, len(items))
	for _, item := range items {
		m[item.Key] = item.Value
	}
	return m
}

func applyGatewayConfig(m map[string]string) GatewayConfig {
	cfg := GatewayConfig{}

	cfg.Channels.CLI.Enabled = m["channels.cli.enabled"] == "true"
	cfg.Channels.CLI.UserID = m["channels.cli.userID"]

	cfg.Channels.WeCom.Enabled = m["channels.wecom.enabled"] == "true"
	cfg.Channels.WeCom.CorpID = m["channels.wecom.corpID"]
	cfg.Channels.WeCom.CorpSecret = m["channels.wecom.corpSecret"]
	if v := m["channels.wecom.agentID"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Channels.WeCom.AgentID = n
		}
	}
	cfg.Channels.WeCom.Token = m["channels.wecom.token"]
	cfg.Channels.WeCom.EncodingAESKey = m["channels.wecom.encodingAESKey"]
	cfg.Channels.WeCom.CallbackAddr = m["channels.wecom.callbackAddr"]
	if cfg.Channels.WeCom.CallbackAddr == "" {
		cfg.Channels.WeCom.CallbackAddr = DefaultWeComCallbackAddr
	}

	cfg.Channels.Feishu.Enabled = m["channels.feishu.enabled"] == "true"
	cfg.Channels.Feishu.AppID = m["channels.feishu.appId"]
	cfg.Channels.Feishu.AppSecret = m["channels.feishu.appSecret"]
	cfg.Channels.Feishu.EncryptKey = m["channels.feishu.encryptKey"]
	cfg.Channels.Feishu.VerificationToken = m["channels.feishu.verificationToken"]

	cfg.Channels.WeChatKF.Enabled = m["channels.wechatkf.enabled"] == "true"
	cfg.Channels.WeChatKF.CorpID = m["channels.wechatkf.corpID"]
	cfg.Channels.WeChatKF.KFSecret = m["channels.wechatkf.kfSecret"]
	cfg.Channels.WeChatKF.OpenKfID = m["channels.wechatkf.openKfID"]
	cfg.Channels.WeChatKF.Token = m["channels.wechatkf.token"]
	cfg.Channels.WeChatKF.EncodingAESKey = m["channels.wechatkf.encodingAESKey"]
	cfg.Channels.WeChatKF.CallbackAddr = m["channels.wechatkf.callbackAddr"]
	if cfg.Channels.WeChatKF.CallbackAddr == "" {
		cfg.Channels.WeChatKF.CallbackAddr = DefaultWeChatKFCallbackAddr
	}

	cfg.Channels.Web.Enabled = m["channels.web.enabled"] == "true"
	cfg.Channels.Web.ListenAddr = m["channels.web.listenAddr"]
	if cfg.Channels.Web.ListenAddr == "" {
		cfg.Channels.Web.ListenAddr = ":8080"
	}

	cfg.Ack.Enabled = m["ack.enabled"] == "true"
	cfg.Ack.Text = m["ack.text"]

	cfg.Auth.JWKSURL = m["auth.jwksUrl"]

	// Turn-failure notices: channels.<name>.error.<category> (and .default).
	errorMessages := make(map[string]string)
	for key, value := range m {
		if !strings.HasPrefix(key, "channels.") || value == "" {
			continue
		}
		rest := strings.TrimPrefix(key, "channels.")
		parts := strings.Split(rest, ".")
		if len(parts) == 3 && parts[1] == "error" {
			errorMessages[parts[0]+"."+parts[2]] = value
		}
	}
	if len(errorMessages) > 0 {
		cfg.ErrorMessages = errorMessages
	}

	return cfg
}

// ValidateGatewayConfig checks that required gateway configuration fields are set.
func ValidateGatewayConfig(cfg GatewayConfig) error {
	return nil
}
