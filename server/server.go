package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/iptecharch/schema-server/config"
	schemapb "github.com/iptecharch/schema-server/protos/schema_server"
	"github.com/iptecharch/schema-server/schema"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip" // Install the gzip compressor
)

type Server struct {
	config *config.Config

	cfn context.CancelFunc

	ms      *sync.RWMutex
	schemas map[string]*schema.Schema

	srv *grpc.Server
	schemapb.UnimplementedSchemaServerServer

	router *mux.Router
	reg    *prometheus.Registry
}

func NewServer(c *config.Config) (*Server, error) {
	ctx, cancel := context.WithCancel(context.TODO())
	var s = &Server{
		config:  c,
		cfn:     cancel,
		ms:      &sync.RWMutex{},
		schemas: make(map[string]*schema.Schema, len(c.Schemas)),
		router:  mux.NewRouter(),
		reg:     prometheus.NewRegistry(),
	}

	// gRPC server options
	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(c.GRPCServer.MaxRecvMsgSize),
	}

	if c.Prometheus != nil {
		grpcClientMetrics := grpc_prometheus.NewClientMetrics()
		s.reg.MustRegister(grpcClientMetrics)

		// add gRPC server interceptors for the Schema/Data server
		grpcMetrics := grpc_prometheus.NewServerMetrics()
		opts = append(opts,
			grpc.StreamInterceptor(grpcMetrics.StreamServerInterceptor()),
		)
		unaryInterceptors := []grpc.UnaryServerInterceptor{
			func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
				ctx, cfn := context.WithTimeout(ctx, c.GRPCServer.RPCTimeout)
				defer cfn()
				return handler(ctx, req)
			},
		}
		unaryInterceptors = append(unaryInterceptors, grpcMetrics.UnaryServerInterceptor())
		opts = append(opts, grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(unaryInterceptors...)))
		s.reg.MustRegister(grpcMetrics)
	}

	if c.GRPCServer.TLS != nil {
		tlsCfg, err := c.GRPCServer.TLS.NewConfig(ctx)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	s.srv = grpc.NewServer(opts...)
	// parse schemas
	for _, sCfg := range c.Schemas {
		sc, err := schema.NewSchema(sCfg)
		if err != nil {
			return nil, fmt.Errorf("schema %s parsing failed: %v", sCfg.Name, err)
		}
		s.schemas[sc.UniqueName("")] = sc
	}
	// register Schema server gRPC Methods
	schemapb.RegisterSchemaServerServer(s.srv, s)
	return s, nil
}

func (s *Server) Serve(ctx context.Context) error {
	l, err := net.Listen("tcp", s.config.GRPCServer.Address)
	if err != nil {
		return err
	}
	log.Infof("running server on %s", s.config.GRPCServer.Address)
	if s.config.Prometheus != nil {
		go s.ServeHTTP()
	}
	err = s.srv.Serve(l)
	if err != nil {
		return err
	}

	return nil
}

func (s *Server) ServeHTTP() {
	s.router.Handle("/metrics", promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{}))
	s.reg.MustRegister(collectors.NewGoCollector())
	s.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	srv := &http.Server{
		Addr:         s.config.Prometheus.Address,
		Handler:      s.router,
		ReadTimeout:  time.Minute,
		WriteTimeout: time.Minute,
	}
	err := srv.ListenAndServe()
	if err != nil {
		log.Errorf("HTTP server stopped: %v", err)
	}
}

func (s *Server) Stop() {
	s.srv.Stop()
	s.cfn()
}

// func (s *Server) BuildSchemaElems(ctx context.Context, sc *schema.Schema) {
// 	sc.Walk(nil, func(ec *yang.Entry) error {
// 		p := make([]string, 0)
// 		ecp := toPath(ec, p)
// 		if ecp == "" {
// 			return nil
// 		}
// 		if _, ok := s.schemaElems[sc.UniqueName("")]; !ok {
// 			s.schemaElems[sc.UniqueName("")] = make(map[string]*schemapb.SchemaElem)
// 		}
// 		s.schemaElems[sc.UniqueName("")][ecp] = schema.SchemaElemFromYEntry(ec, true)
// 		// log.Debugf("storing %q under %q", ec.Name, ecp)
// 		return nil
// 	})
// }

// func toPath(e *yang.Entry, p []string) string {
// 	if e.Annotation != nil && e.Annotation["root"] == true {
// 		reverse(p)
// 		return strings.Join(p, "/")
// 	}
// 	if e.IsCase() || e.IsChoice() {
// 		e = e.Parent
// 	}
// 	p = append(p, e.Name)
// 	if e.Parent != nil {
// 		if e.Parent.IsCase() || e.Parent.IsChoice() {
// 			return toPath(e.Parent.Parent, p)
// 		}
// 		return toPath(e.Parent, p)
// 	}
// 	reverse(p)
// 	return strings.Join(p[1:], "/")
// }

// func reverse(p []string) {
// 	for i, j := 0, len(p)-1; i < j; i, j = i+1, j-1 {
// 		p[i], p[j] = p[j], p[i]
// 	}
// }
