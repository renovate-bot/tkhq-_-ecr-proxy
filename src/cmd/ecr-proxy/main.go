package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	DefaultRegion = "us-east-1"
)

var (
	ecrAccount string
	region     string
	listenAddr string

	tlsCert string
	tlsKey  string
	tlsAddr string

	debug bool

	ecrClient *ecr.Client
	authData  *types.AuthorizationData

	TokenCheckInterval = time.Minute

	log *zap.Logger
)

func init() {
	flag.StringVar(&listenAddr, "addr", ":8080", "listen address for HTTP proxy")
	flag.StringVar(&ecrAccount, "account", "", "aws account for the ECR registry")
	flag.StringVar(&region, "region", DefaultRegion, "region in which the ECR registry is located")
	flag.StringVar(&tlsAddr, "tls-addr", ":8443", "listen address for HTTPS proxy")
	flag.StringVar(&tlsCert, "tls-cert", tlsCert, "certificate file for TLS")
	flag.StringVar(&tlsKey, "tls-key", tlsKey, "key file for TLS")
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
}

func main() {
	var err error

	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if debug {
		log, err = zap.NewDevelopment()
	} else {
		log, err = zap.NewProduction()
	}
	if err != nil {
		fmt.Println("failed to instantiate logger:", err)

		os.Exit(127)
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
	)
	if err != nil {
		log.Fatal("failed to load AWS credential configuration", zap.Error(err))
	}

	if ecrAccount == "" {
		log.Fatal("ECR account must be specified")
	}

	ecrClient = ecr.NewFromConfig(cfg)

	authData, err = ensureToken(ctx, ecrClient, authData)
	if err != nil {
		log.Fatal("failed to get ECR authorization token", zap.Error(err))
	}

	go maintainToken(ctx, ecrClient)

	mux := http.NewServeMux()

	proxyLogger, err := zap.NewStdLogAt(log, zap.DebugLevel)
	if err != nil {
		log.Fatal("failed to create a standard logger for the reverse proxy", zap.Error(err))
	}

	mux.Handle("/v2/", &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			if err := addAuthToken(req); err != nil {
				log.Error("failed to add auth token to request", zap.Error(err))
			}
		},
		ErrorLog: proxyLogger,
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		if authData == nil || authData.ExpiresAt.Before(time.Now()) {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusOK)
	})

	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal("failed to listen on HTTP port", zap.String("addr", listenAddr), zap.Error(err))
	}

	go func() {
		<-ctx.Done()

		l.Close() //nolint:errcheck
	}()

	if tlsKey != "" && tlsCert != "" {
		secureListener, err := net.Listen("tcp", tlsAddr)
		if err != nil {
			log.Fatal("failed to listen on HTTPS port", zap.String("addr", tlsAddr), zap.Error(err))
		}

		go func() {
			<-ctx.Done()

			secureListener.Close() //nolint:errcheck
		}()

		go func() {
			if err = http.ServeTLS(secureListener, mux, tlsCert, tlsKey); err != nil {
				log.Fatal("HTTPS listener exited", zap.Error(err))
			}
		}()
	}

	err = http.Serve(l, mux)

	log.Fatal("HTTP listener exited", zap.Error(err))
}

func addAuthToken(req *http.Request) error {
	ecrEndpoint, err := url.Parse(aws.ToString(authData.ProxyEndpoint))
	if err != nil {
		return errors.Wrap(err, "failed to parse AWS ECR proxy endpoint")
	}

	req.URL.Scheme = ecrEndpoint.Scheme
	req.URL.Host = ecrEndpoint.Host
	req.Host = ecrEndpoint.Host

	req.Header.Set("Authorization", fmt.Sprintf("Basic %s", aws.ToString(authData.AuthorizationToken)))

	return nil
}

func maintainToken(ctx context.Context, ecrClient *ecr.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(TokenCheckInterval):
		}

		newToken, err := ensureToken(ctx, ecrClient, authData)
		if err != nil {
			log.Error("failed to update token", zap.Error(err))

			continue
		}

		authData = newToken
	}
}

func ensureToken(ctx context.Context, ecrClient *ecr.Client, existingAuth *types.AuthorizationData) (*types.AuthorizationData, error) {
	if existingAuth != nil && existingAuth.ExpiresAt.After(time.Now().Add(time.Hour)) {
		return existingAuth, nil
	}

	resp, err := ecrClient.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{
		RegistryIds: []string{ecrAccount},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.AuthorizationData) < 1 {
		return nil, fmt.Errorf("no authorization data found in AWS response")
	}

	return &(resp.AuthorizationData[0]), nil
}
