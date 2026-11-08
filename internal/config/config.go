// Package config defines configuration for the nanogateway service.
package config

// GatewayConfig holds configuration for the gateway service.
type GatewayConfig struct {
	Channels ChannelsConfig `json:"channels" mapstructure:"channels"`
	Ack      AckConfig      `json:"ack"      mapstructure:"ack"`
	Auth     AuthConfig     `json:"auth"     mapstructure:"auth"`
	// ErrorMessages maps "<channel>.<category>" (or "<channel>.default") to the
	// user-facing notice sent to batch-channel users when a turn fails.
	// Categories are the nanoprotocol.TurnError* values. Built-in per-language
	// defaults apply for keys that are absent.
	ErrorMessages map[string]string `json:"errorMessages" mapstructure:"errorMessages"`
}

// AuthConfig holds JWT authentication settings.
type AuthConfig struct {
	JWKSURL string `json:"jwksUrl" mapstructure:"jwksUrl"`
}

// AckConfig holds settings for immediate message acknowledgement.
type AckConfig struct {
	Enabled bool   `json:"enabled" mapstructure:"enabled"`
	Text    string `json:"text"    mapstructure:"text"`
}

// ChannelsConfig holds settings for all channel adapters.
type ChannelsConfig struct {
	CLI      CLIChannelConfig      `json:"cli"      mapstructure:"cli"`
	WeCom    WeComChannelConfig    `json:"wecom"    mapstructure:"wecom"`
	Feishu   FeishuChannelConfig   `json:"feishu"   mapstructure:"feishu"`
	WeChatKF WeChatKFChannelConfig `json:"wechatkf" mapstructure:"wechatkf"`
	Web      WebChannelConfig      `json:"web"      mapstructure:"web"`
}

// CLIChannelConfig holds CLI adapter settings.
type CLIChannelConfig struct {
	Enabled bool   `json:"enabled" mapstructure:"enabled"`
	UserID  string `json:"userID"  mapstructure:"userID"`
}

// WeComChannelConfig holds WeCom (Enterprise WeChat) adapter settings.
type WeComChannelConfig struct {
	Enabled        bool   `json:"enabled"        mapstructure:"enabled"`
	CorpID         string `json:"corpID"         mapstructure:"corpID"`
	CorpSecret     string `json:"corpSecret"     mapstructure:"corpSecret"`
	AgentID        int64  `json:"agentID"        mapstructure:"agentID"`
	Token          string `json:"token"          mapstructure:"token"`
	EncodingAESKey string `json:"encodingAESKey" mapstructure:"encodingAESKey"`
	CallbackAddr   string `json:"callbackAddr"   mapstructure:"callbackAddr"`
}

// FeishuChannelConfig holds Feishu adapter settings.
type FeishuChannelConfig struct {
	Enabled           bool   `json:"enabled"           mapstructure:"enabled"`
	AppID             string `json:"appId"             mapstructure:"appId"`
	AppSecret         string `json:"appSecret"         mapstructure:"appSecret"`
	EncryptKey        string `json:"encryptKey"        mapstructure:"encryptKey"`
	VerificationToken string `json:"verificationToken" mapstructure:"verificationToken"`
}

// WeChatKFChannelConfig holds WeChat Customer Service (KF) adapter settings.
type WeChatKFChannelConfig struct {
	Enabled        bool   `json:"enabled"        mapstructure:"enabled"`
	CorpID         string `json:"corpID"         mapstructure:"corpID"`
	KFSecret       string `json:"kfSecret"       mapstructure:"kfSecret"`
	OpenKfID       string `json:"openKfID"       mapstructure:"openKfID"`
	Token          string `json:"token"          mapstructure:"token"`
	EncodingAESKey string `json:"encodingAESKey" mapstructure:"encodingAESKey"`
	CallbackAddr   string `json:"callbackAddr"   mapstructure:"callbackAddr"`
}

// WebChannelConfig holds the browser web chat adapter settings.
type WebChannelConfig struct {
	Enabled    bool   `json:"enabled"    mapstructure:"enabled"`
	ListenAddr string `json:"listenAddr" mapstructure:"listenAddr"`
}
