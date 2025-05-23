package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/fullstorydev/grpchan/inprocgrpc"
	authnlib "github.com/grafana/authlib/authn"
	authzv1 "github.com/grafana/authlib/authz/proto/v1"
	"github.com/grafana/authlib/claims"
	"github.com/grafana/dskit/services"
	grpcAuth "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/grafana/grafana/pkg/infra/db"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/tracing"
	authzextv1 "github.com/grafana/grafana/pkg/services/authz/proto/v1"
	"github.com/grafana/grafana/pkg/services/authz/zanzana"
	zclient "github.com/grafana/grafana/pkg/services/authz/zanzana/client"
	zserver "github.com/grafana/grafana/pkg/services/authz/zanzana/server"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/grpcserver"
	"github.com/grafana/grafana/pkg/services/grpcserver/interceptors"
	"github.com/grafana/grafana/pkg/setting"
)

const zanzanaAudience = "zanzana"

// ProvideZanzana used to register ZanzanaClient.
// It will also start an embedded ZanzanaSever if mode is set to "embedded".
func ProvideZanzana(cfg *setting.Cfg, db db.DB, features featuremgmt.FeatureToggles) (zanzana.Client, error) {
	if !features.IsEnabledGlobally(featuremgmt.FlagZanzana) {
		return zclient.NewNoop(), nil
	}

	logger := log.New("zanzana")

	var client zanzana.Client
	switch cfg.Zanzana.Mode {
	case setting.ZanzanaModeClient:
		tokenClient, err := authnlib.NewTokenExchangeClient(authnlib.TokenExchangeConfig{
			Token:            cfg.Zanzana.Token,
			TokenExchangeURL: cfg.Zanzana.TokenExchangeURL,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize token exchange client: %w", err)
		}

		if cfg.StackID == "" {
			return nil, fmt.Errorf("missing stack ID")
		}
		namespace := fmt.Sprintf("stacks-%s", cfg.StackID)

		tokenAuthCred := &tokenAuth{
			cfg:         cfg,
			namespace:   namespace,
			tokenClient: tokenClient,
		}

		dialOptions := []grpc.DialOption{
			// TODO: add TLS support
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(tokenAuthCred),
		}

		conn, err := grpc.NewClient(cfg.Zanzana.Addr, dialOptions...)
		if err != nil {
			return nil, fmt.Errorf("failed to create zanzana client to remote server: %w", err)
		}

		client, err = zclient.NewClient(context.Background(), conn, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize zanzana client: %w", err)
		}
	case setting.ZanzanaModeEmbedded:
		store, err := zanzana.NewEmbeddedStore(cfg, db, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to start zanzana: %w", err)
		}

		openfga, err := zserver.NewOpenFGA(&cfg.Zanzana, store, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to start zanzana: %w", err)
		}

		srv, err := zserver.NewAuthzServer(cfg, openfga)
		if err != nil {
			return nil, fmt.Errorf("failed to start zanzana: %w", err)
		}

		channel := &inprocgrpc.Channel{}
		// Put * as a namespace so we can properly authorize request with in-proc mode
		channel.WithServerUnaryInterceptor(grpcAuth.UnaryServerInterceptor(func(ctx context.Context) (context.Context, error) {
			ctx = claims.WithClaims(ctx, authnlib.NewAccessTokenAuthInfo(authnlib.Claims[authnlib.AccessTokenClaims]{
				Rest: authnlib.AccessTokenClaims{
					Namespace: "*",
				},
			}))
			return ctx, nil
		}))

		openfgav1.RegisterOpenFGAServiceServer(channel, openfga)
		authzv1.RegisterAuthzServiceServer(channel, srv)
		authzextv1.RegisterAuthzExtentionServiceServer(channel, srv)

		client, err = zclient.NewClient(context.Background(), channel, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize zanzana client: %w", err)
		}

	default:
		return nil, fmt.Errorf("unsupported zanzana mode: %s", cfg.Zanzana.Mode)
	}

	return client, nil
}

type ZanzanaService interface {
	services.NamedService
}

var _ ZanzanaService = (*Zanzana)(nil)

// ProvideZanzanaService is used to register zanzana as a module so we can run it seperatly from grafana.
func ProvideZanzanaService(cfg *setting.Cfg, features featuremgmt.FeatureToggles) (*Zanzana, error) {
	s := &Zanzana{
		cfg:      cfg,
		features: features,
		logger:   log.New("zanzana"),
	}

	s.BasicService = services.NewBasicService(s.start, s.running, s.stopping).WithName("zanzana")

	return s, nil
}

type Zanzana struct {
	*services.BasicService

	cfg *setting.Cfg

	logger   log.Logger
	handle   grpcserver.Provider
	features featuremgmt.FeatureToggles
}

func (z *Zanzana) start(ctx context.Context) error {
	store, err := zanzana.NewStore(z.cfg, z.logger)
	if err != nil {
		return fmt.Errorf("failed to initilize zanana store: %w", err)
	}

	openfga, err := zserver.NewOpenFGA(&z.cfg.Zanzana, store, z.logger)
	if err != nil {
		return fmt.Errorf("failed to start zanzana: %w", err)
	}

	srv, err := zserver.NewAuthzServer(z.cfg, openfga)
	if err != nil {
		return fmt.Errorf("failed to start zanzana: %w", err)
	}

	tracingCfg, err := tracing.ProvideTracingConfig(z.cfg)
	if err != nil {
		return err
	}
	tracingCfg.ServiceName = "zanzana"

	tracer, err := tracing.ProvideService(tracingCfg)
	if err != nil {
		return err
	}

	authenticator := authnlib.NewAccessTokenAuthenticator(
		authnlib.NewAccessTokenVerifier(
			authnlib.VerifierConfig{
				AllowedAudiences: []string{zanzanaAudience},
			},
			authnlib.NewKeyRetriever(authnlib.KeyRetrieverConfig{
				SigningKeysURL: z.cfg.Zanzana.SigningKeysURL,
			}),
		),
	)

	authfn := interceptors.AuthenticatorFunc(func(ctx context.Context) (context.Context, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, fmt.Errorf("missing metadata")
		}
		c, err := authenticator.Authenticate(ctx, authnlib.NewGRPCTokenProvider(md))
		if err != nil {
			return nil, err
		}
		return claims.WithClaims(ctx, c), nil
	})

	z.handle, err = grpcserver.ProvideService(z.cfg, z.features, authfn, tracer, prometheus.DefaultRegisterer)
	if err != nil {
		return fmt.Errorf("failed to create zanzana grpc server: %w", err)
	}

	s := z.handle.GetServer()
	openfgav1.RegisterOpenFGAServiceServer(s, openfga)
	authzv1.RegisterAuthzServiceServer(s, srv)
	authzextv1.RegisterAuthzExtentionServiceServer(s, srv)

	if _, err := grpcserver.ProvideReflectionService(z.cfg, z.handle); err != nil {
		return fmt.Errorf("failed to register reflection for zanzana: %w", err)
	}

	return nil
}

func (z *Zanzana) running(ctx context.Context) error {
	if z.cfg.Env == setting.Dev && z.cfg.Zanzana.ListenHTTP {
		go func() {
			z.logger.Info("Starting OpenFGA HTTP server")
			err := zserver.StartOpenFGAHttpSever(z.cfg, z.handle, z.logger)
			if err != nil {
				z.logger.Error("failed to start OpenFGA HTTP server", "error", err)
			}
		}()
	}

	// Run is blocking so we can just run it here
	return z.handle.Run(ctx)
}

func (z *Zanzana) stopping(err error) error {
	if err != nil && !errors.Is(err, context.Canceled) {
		z.logger.Error("Stopping zanzana due to unexpected error", "err", err)
	}
	return nil
}

type tokenAuth struct {
	cfg         *setting.Cfg
	namespace   string
	tokenClient *authnlib.TokenExchangeClient
}

func (t *tokenAuth) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token, err := t.tokenClient.Exchange(ctx, authnlib.TokenExchangeRequest{
		Namespace: t.namespace,
		Audiences: []string{zanzanaAudience},
	})
	if err != nil {
		return nil, err
	}

	return map[string]string{
		authnlib.DefaultAccessTokenMetadataKey: token.Token,
	}, nil
}

func (t *tokenAuth) RequireTransportSecurity() bool {
	return false
}
