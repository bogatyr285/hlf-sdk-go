package util

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"strconv"
	"time"

	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	"github.com/pkg/errors"
	"go.opencensus.io/plugin/ocgrpc"
	"go.opencensus.io/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	"github.com/s7techlab/hlf-sdk-go/api/config"
	"github.com/s7techlab/hlf-sdk-go/opencensus/hlf"
)

var (
	DefaultGRPCRetryConfig = &config.GRPCRetryConfig{
		Max:     10,
		Timeout: config.Duration{Duration: 10 * time.Second},
	}

	DefaultGRPCKeepAliveConfig = &config.GRPCKeepAliveConfig{
		Time:    60,
		Timeout: 5,
	}
)

const (
	maxRecvMsgSize = 100 * 1024 * 1024
	maxSendMsgSize = 100 * 1024 * 1024
)

func NewGRPCOptionsFromConfig(c config.ConnectionConfig, log *zap.Logger) ([]grpc.DialOption, error) {

	// TODO: move to config or variable options
	grpcOptions := []grpc.DialOption{
		grpc.WithStatsHandler(hlf.Wrap(&ocgrpc.ClientHandler{
			StartOptions: trace.StartOptions{
				Sampler:  trace.AlwaysSample(),
				SpanKind: trace.SpanKindClient,
			},
		})),
	}

	if c.Tls.Enabled {
		var err error
		var tlsCfg tls.Config
		tlsCfg.InsecureSkipVerify = c.Tls.SkipVerify
		// if custom CA certificate is presented, use it
		if c.Tls.CACertPath != `` {
			caCert, err := ioutil.ReadFile(c.Tls.CACertPath)
			if err != nil {
				return nil, errors.Wrap(err, `failed to read CA certificate`)
			}
			certPool := x509.NewCertPool()
			if ok := certPool.AppendCertsFromPEM(caCert); !ok {
				return nil, errors.New(`failed to append CA certificate to chain`)
			}
			tlsCfg.RootCAs = certPool
		} else {
			// otherwise we use system certificates
			if tlsCfg.RootCAs, err = x509.SystemCertPool(); err != nil {
				return nil, errors.Wrap(err, `failed to get system cert pool`)
			}
		}
		if c.Tls.CertPath != `` {
			// use mutual tls if certificate and pk is presented
			if c.Tls.KeyPath != `` {
				cert, err := tls.LoadX509KeyPair(c.Tls.CertPath, c.Tls.KeyPath)
				if err != nil {
					return nil, errors.Wrap(err, `failed to load client certificate`)
				}
				tlsCfg.Certificates = append(tlsCfg.Certificates, cert)
			}
		}

		cred := credentials.NewTLS(&tlsCfg)
		grpcOptions = append(grpcOptions, grpc.WithTransportCredentials(cred))
	} else {

		grpcOptions = append(grpcOptions, grpc.WithInsecure())
	}

	// Set default keep alive
	if c.GRPC.KeepAlive == nil {
		c.GRPC.KeepAlive = DefaultGRPCKeepAliveConfig
	}
	grpcOptions = append(grpcOptions, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                time.Duration(c.GRPC.KeepAlive.Time) * time.Second,
		Timeout:             time.Duration(c.GRPC.KeepAlive.Timeout) * time.Second,
		PermitWithoutStream: true,
	}))

	var retryConfig *config.GRPCRetryConfig
	if c.GRPC.Retry != nil {
		retryConfig = c.GRPC.Retry
	} else if c.Timeout.String() != `` {
		retryConfig = DefaultGRPCRetryConfig
		retryConfig.Timeout = c.Timeout
	} else {
		retryConfig = DefaultGRPCRetryConfig
	}

	grpcOptions = append(grpcOptions,
		grpc.WithUnaryInterceptor(
			grpc_retry.UnaryClientInterceptor(
				grpc_retry.WithMax(retryConfig.Max),
				grpc_retry.WithBackoff(grpc_retry.BackoffLinear(retryConfig.Timeout.Duration)),
			),
		),
	)

	grpcOptions = append(grpcOptions, grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(maxRecvMsgSize),
		grpc.MaxCallSendMsgSize(maxSendMsgSize),
	))

	fields := []zap.Field{
		zap.String(`host`, c.Host),
		zap.Bool(`tls`, c.Tls.Enabled),
		zap.Reflect(`keep alive`, c.GRPC.KeepAlive),
		zap.Reflect(`retry`, retryConfig),
	}
	if c.Tls.Enabled {
		fields = append(fields, zap.Reflect(`retry`, c.Tls))
	}

	log.Debug(`grpc options for host`, fields...)
	grpcOptions = append(grpcOptions, grpc.WithBlock())

	return grpcOptions, nil
}

func NewGRPCConnectionFromConfigs(ctx context.Context, log *zap.Logger, conf ...config.ConnectionConfig) (*grpc.ClientConn, error) {
	// use options from first config
	opts, err := NewGRPCOptionsFromConfig(conf[0], log)
	if err != nil {
		return nil, errors.Wrap(err, `failed to get GRPC options`)
	}

	var (
		addrs = make([]resolver.Address, len(conf))
		hosts []string
	)
	for i, cc := range conf {
		addrs[i] = resolver.Address{Addr: cc.Host}
		hosts = append(hosts, cc.Host)
	}

	// from removed manual.GenerateAndRegisterManualResolver
	// todo: rewrite with grpc.WithResolvers
	scheme := strconv.FormatInt(time.Now().UnixNano(), 36)
	r := manual.NewBuilderWithScheme(scheme)
	r.InitialState(resolver.State{Addresses: addrs})
	resolver.Register(r)

	opts = append(opts, grpc.WithBalancerName(roundrobin.Name))

	log.Debug(`grpc dial to orderer`, zap.Strings(`hosts`, hosts))

	conn, err := grpc.DialContext(ctx, fmt.Sprintf("%s:///%s", r.Scheme(), `orderers`), opts...)
	if err != nil {
		return nil, errors.Wrap(err, `failed to initialize GRPC connection`)
	}

	return conn, nil
}
