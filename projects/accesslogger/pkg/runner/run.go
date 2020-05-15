package runner

import (
	"context"
	"fmt"
	"net"

	"github.com/solo-io/gloo/pkg/utils"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v2"
	"github.com/solo-io/gloo/projects/accesslogger/pkg/loggingservice"
	"github.com/solo-io/go-utils/contextutils"
	"github.com/solo-io/go-utils/healthchecker"
	"github.com/solo-io/go-utils/stats"
	"go.opencensus.io/plugin/ocgrpc"
	ocstats "go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

func init() {
	view.Register(ocgrpc.DefaultServerViews...)
	view.Register(accessLogsRequestsView)
}

var (
	mAccessLogsRequests    = ocstats.Int64("accesslogs.gloo.solo.io/accesslogs/requests", "The number of requests", "1")
	requestPathKey, _      = tag.NewKey("request_path")
	responseCodeKey, _     = tag.NewKey("response_code")
	clusterKey, _          = tag.NewKey("cluster")
	accessLogsRequestsView = &view.View{
		Name:        "accesslogs.gloo.solo.io/accesslogs/requests",
		Measure:     mAccessLogsRequests,
		Description: "The number of requests, as noted in access logs (which are lossy)",
		Aggregation: view.Count(),
		TagKeys:     []tag.Key{requestPathKey, responseCodeKey, clusterKey},
	}
)

func Run() {
	clientSettings := NewSettings()
	ctx := contextutils.WithLogger(context.Background(), "access_log")

	if clientSettings.DebugPort != 0 {
		// TODO(yuval-k): we need to start the stats server before calling contextutils
		// need to think of a better way to express this dependency, or preferably, fix it.
		stats.StartStatsServerWithPort(stats.StartupOptions{Port: clientSettings.DebugPort})
	}

	opts := loggingservice.Options{
		Callbacks: loggingservice.AlsCallbackList{
			func(ctx context.Context, message *pb.StreamAccessLogsMessage) error {
				logger := contextutils.LoggerFrom(ctx)
				switch msg := message.GetLogEntries().(type) {
				case *pb.StreamAccessLogsMessage_HttpLogs:
					for _, v := range msg.HttpLogs.LogEntry {

						requestPath := v.GetRequest().GetPath()
						requestOrigPath := v.GetRequest().GetOriginalPath()
						requestMethod := v.GetRequest().GetRequestMethod()
						responseCode := v.GetResponse().GetResponseCode()
						cluster := v.GetCommonProperties().GetUpstreamCluster()
						routeName := v.GetCommonProperties().GetRouteName()
						startTime := v.GetCommonProperties().GetStartTime()
						timeToLastUpstreamTxByte := v.GetCommonProperties().GetTimeToLastUpstreamTxByte()

						utils.MeasureOne(
							ctx,
							mAccessLogsRequests,
							tag.Insert(requestPathKey, requestPath),
							tag.Insert(responseCodeKey, string(responseCode.GetValue())),
							tag.Insert(clusterKey, cluster))

						logger.With(
							zap.Any("protocol_version", v.GetProtocolVersion()),
							zap.Any("request_path", requestPath),
							zap.Any("request_original_path", requestOrigPath),
							zap.Any("request_method", requestMethod),
							zap.Any("response_code", responseCode.GetValue()),
							zap.Any("cluster", cluster),
							zap.Any("route_name", routeName),
							zap.Any("start_time", startTime),
							zap.Any("time_to_last_upstream_tx_byte", timeToLastUpstreamTxByte),
						).Info("received http request")
					}
				case *pb.StreamAccessLogsMessage_TcpLogs:
					for _, v := range msg.TcpLogs.LogEntry {
						logger.With(
							zap.Any("upstream_cluster", v.GetCommonProperties().GetUpstreamCluster()),
							zap.Any("route_name", v.GetCommonProperties().GetRouteName()),
						).Info("received tcp request")
					}
				}
				return nil
			},
		},
		Ctx: ctx,
	}
	service := loggingservice.NewServer(opts)

	err := RunWithSettings(ctx, service, clientSettings)

	if err != nil {
		if ctx.Err() == nil {
			// not a context error - panic
			panic(err)
		}
	}
}

func RunWithSettings(ctx context.Context, service *loggingservice.Server, clientSettings Settings) error {
	err := StartAccessLog(ctx, clientSettings, service)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func StartAccessLog(ctx context.Context, clientSettings Settings, service *loggingservice.Server) error {
	srv := grpc.NewServer(grpc.StatsHandler(&ocgrpc.ServerHandler{}))

	pb.RegisterAccessLogServiceServer(srv, service)
	hc := healthchecker.NewGrpc(clientSettings.ServiceName, health.NewServer())
	healthpb.RegisterHealthServer(srv, hc.GetServer())
	reflection.Register(srv)

	logger := contextutils.LoggerFrom(ctx)
	logger.Infow("Starting access-log server")

	addr := fmt.Sprintf(":%d", clientSettings.ServerPort)
	runMode := "gRPC"
	network := "tcp"

	logger.Infof("access-log server running in [%s] mode, listening at [%s]", runMode, addr)
	lis, err := net.Listen(network, addr)
	if err != nil {
		logger.Errorw("Failed to announce on network", zap.Any("mode", runMode), zap.Any("address", addr), zap.Any("error", err))
		return err
	}
	go func() {
		<-ctx.Done()
		srv.Stop()
		_ = lis.Close()
	}()

	return srv.Serve(lis)
}
