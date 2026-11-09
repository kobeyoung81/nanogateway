// Package main provides the nanogateway CLI binary.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gitee.com/kobeyoung81/nanogateway/internal/auth"
	"gitee.com/kobeyoung81/nanogateway/internal/channel"
	"gitee.com/kobeyoung81/nanogateway/internal/channel/cli"
	"gitee.com/kobeyoung81/nanogateway/internal/channel/feishu"
	"gitee.com/kobeyoung81/nanogateway/internal/channel/web"
	"gitee.com/kobeyoung81/nanogateway/internal/channel/wechatkf"
	"gitee.com/kobeyoung81/nanogateway/internal/channel/wecom"
	"gitee.com/kobeyoung81/nanogateway/internal/config"
	"gitee.com/kobeyoung81/nanogateway/internal/gateway"
	gwlog "gitee.com/kobeyoung81/nanogateway/internal/log"
	"github.com/spf13/cobra"
)

// Build-time variables set via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	setupLogger()

	var busURL string
	var message string

	rootCmd := &cobra.Command{
		Use:   "nanogateway",
		Short: "Nanogateway - platform adapter service",
		Long: `Nanogateway connects to chat platforms (CLI, WeCom, Feishu, WeChat KF)
and routes messages to/from the bus. It handles user identity resolution
and outbound message delivery.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedBusURL := config.ResolveBusURL(busURL)

			cfg, err := config.LoadGatewayConfig(config.AdminURL())
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := config.ValidateGatewayConfig(cfg); err != nil {
				return fmt.Errorf("validate config: %w", err)
			}

			var jwtValidator *auth.JWKSValidator
			if cfg.Auth.JWKSURL != "" {
				jwtValidator = auth.NewJWKSValidator(cfg.Auth.JWKSURL)
				slog.Info("JWT validation enabled", "jwks_url", cfg.Auth.JWKSURL)
			}

			slog.Info("starting gateway service", "bus_url", resolvedBusURL)

			var channels []channel.Channel

			if cfg.Channels.CLI.Enabled {
				slog.Info("enabling cli channel", "user_id", cfg.Channels.CLI.UserID)
				cliAdapter := cli.New(cfg.Channels.CLI.UserID)
				if message != "" {
					cliAdapter.SetOneShotMessage(message)
				}
				channels = append(channels, cliAdapter)
			}

			if cfg.Channels.WeCom.Enabled {
				slog.Info("enabling wecom channel", "corp_id", cfg.Channels.WeCom.CorpID)
				wecomAdapter, err := wecom.New(
					cfg.Channels.WeCom.CorpID,
					cfg.Channels.WeCom.CorpSecret,
					cfg.Channels.WeCom.AgentID,
					cfg.Channels.WeCom.Token,
					cfg.Channels.WeCom.EncodingAESKey,
					cfg.Channels.WeCom.CallbackAddr,
				)
				if err != nil {
					return fmt.Errorf("create wecom adapter: %w", err)
				}
				channels = append(channels, wecomAdapter)
			}

			if cfg.Channels.Feishu.Enabled {
				slog.Info("enabling feishu channel", "app_id", cfg.Channels.Feishu.AppID)
				feishuAdapter, err := feishu.New(
					cfg.Channels.Feishu.AppID,
					cfg.Channels.Feishu.AppSecret,
					cfg.Channels.Feishu.EncryptKey,
					cfg.Channels.Feishu.VerificationToken,
				)
				if err != nil {
					return fmt.Errorf("create feishu adapter: %w", err)
				}
				channels = append(channels, feishuAdapter)
			}

			if cfg.Channels.WeChatKF.Enabled {
				slog.Info("enabling wechatkf channel", "corp_id", cfg.Channels.WeChatKF.CorpID)
				kfAdapter, err := wechatkf.New(
					cfg.Channels.WeChatKF.CorpID,
					cfg.Channels.WeChatKF.KFSecret,
					cfg.Channels.WeChatKF.OpenKfID,
					cfg.Channels.WeChatKF.Token,
					cfg.Channels.WeChatKF.EncodingAESKey,
					cfg.Channels.WeChatKF.CallbackAddr,
					resolvedBusURL,
				)
				if err != nil {
					return fmt.Errorf("create wechatkf adapter: %w", err)
				}
				channels = append(channels, kfAdapter)
			}

			if cfg.Channels.Web.Enabled {
				slog.Info("enabling web channel", "listen_addr", cfg.Channels.Web.ListenAddr)
				webAdapter := web.New(resolvedBusURL, cfg.Channels.Web.ListenAddr)
				channels = append(channels, webAdapter)
			}

			if len(channels) == 0 {
				return fmt.Errorf("no channels enabled")
			}

			gw := gateway.New(resolvedBusURL, channels, cfg.Ack.Enabled, cfg.Ack.Text, jwtValidator)
			gw.SetErrorMessages(cfg.ErrorMessages)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				sig := <-sigChan
				slog.Info("received signal, shutting down", "signal", sig)
				cancel()
			}()

			if err := gw.Run(ctx); err != nil && err != context.Canceled {
				return fmt.Errorf("gateway run: %w", err)
			}

			slog.Info("gateway stopped")
			return nil
		},
	}

	rootCmd.Flags().StringVarP(&busURL, "bus-url", "b", "", "bus WebSocket URL (env: NANOSEC_GATEWAY_BUS_URL)")
	rootCmd.Flags().StringVarP(&message, "message", "m", "", "send a single message and exit (CLI channel only)")
	rootCmd.AddCommand(newVersionCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func setupLogger() {
	level := slog.LevelInfo
	if env := os.Getenv("LOG_LEVEL"); env != "" {
		switch strings.ToUpper(env) {
		case "VERBOSE":
			level = gwlog.LevelVerbose
		case "DEBUG":
			level = slog.LevelDebug
		case "WARN":
			level = slog.LevelWarn
		case "ERROR":
			level = slog.LevelError
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("nanogateway %s (commit: %s, built: %s)\n", version, commit, date)
		},
	}
}
