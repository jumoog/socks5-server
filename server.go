package main

import (
	"net/netip"
	"os"

	"jumoog/socks5-server/go-socks5"

	"github.com/caarlos0/env/v11"
	"github.com/sirupsen/logrus"
)

type params struct {
	User            string   `env:"PROXY_USER" envDefault:""`
	Password        string   `env:"PROXY_PASSWORD" envDefault:""`
	Port            string   `env:"PROXY_PORT" envDefault:"1080"`
	AllowedDestFqdn string   `env:"ALLOWED_DEST_FQDN" envDefault:""`
	AllowedIPs      []string `env:"ALLOWED_IPS" envSeparator:"," envDefault:""`
}

func main() {
	// Working with app params
	cfg := params{}
	err := env.Parse(&cfg)
	if err != nil {
		logrus.Fatalf("%+v\n", err)
	}

	//Initialize socks5 config
	socks5conf := &socks5.Config{}

	if cfg.User+cfg.Password != "" {
		creds := socks5.StaticCredentials{
			os.Getenv("PROXY_USER"): os.Getenv("PROXY_PASSWORD"),
		}
		cator := socks5.UserPassAuthenticator{Credentials: creds}
		socks5conf.AuthMethods = []socks5.Authenticator{cator}
	}

	if cfg.AllowedDestFqdn != "" {
		socks5conf.Rules = PermitDestAddrPattern(cfg.AllowedDestFqdn)
	}

	server, err := socks5.New(socks5conf)
	if err != nil {
		logrus.Fatal(err)
	}

	// Set IP whitelist
	if len(cfg.AllowedIPs) > 0 {
		whitelist := make([]netip.Addr, len(cfg.AllowedIPs))
		for i, ip := range cfg.AllowedIPs {
			whitelist[i], _ = netip.ParseAddr(ip)
		}
		server.SetIPWhitelist(whitelist)
	}

	logrus.Infof("Start listening proxy service on port %s\n", cfg.Port)
	if err := server.ListenAndServe("tcp", ":"+cfg.Port); err != nil {
		logrus.Fatal(err)
	}
}
