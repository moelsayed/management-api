package cluster

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"sync"
	"time"

	clusterapi "github.com/rancher/cluster-api/server"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/rancher/types/client/management/v3"
	"github.com/rancher/types/config"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/rest"
)

type Manager struct {
	ClusterLister    v3.ClusterLister
	ManagementConfig rest.Config
	LocalConfig      *rest.Config
	servers          sync.Map
}

type record struct {
	handler http.Handler
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewManager(management *config.ManagementContext) *Manager {
	return &Manager{
		ClusterLister:    management.Management.Clusters("").Controller().Lister(),
		ManagementConfig: management.RESTConfig,
		LocalConfig:      management.LocalConfig,
	}
}

func (c *Manager) APIServer(ctx context.Context, cluster *client.Cluster) http.Handler {
	obj, ok := c.servers.Load(cluster.Uuid)
	if ok {
		return obj.(*record).handler
	}

	server, err := c.toServer(cluster)
	if server == nil || err != nil {
		if err != nil {
			logrus.Errorf("Failed to load cluster %s: %v", cluster.ID, err)
		}
		return nil
	}

	obj, loaded := c.servers.LoadOrStore(cluster.Uuid, server)
	if !loaded {
		go func() {
			time.Sleep(10 * time.Minute)
			c.servers.Delete(cluster.Uuid)
			time.Sleep(time.Minute)
			obj.(*record).cancel()
		}()
	}

	return obj.(*record).handler
}

func (c *Manager) toRESTConfig(publicCluster *client.Cluster) (*rest.Config, error) {
	cluster, err := c.ClusterLister.Get("", publicCluster.ID)
	if err != nil {
		return nil, err
	}

	if cluster == nil {
		return nil, nil
	}

	if cluster.Spec.Internal {
		return c.LocalConfig, nil
	}

	if cluster.Status.APIEndpoint == "" || cluster.Status.CACert == "" || cluster.Status.ServiceAccountToken == "" {
		return nil, nil
	}

	u, err := url.Parse(cluster.Status.APIEndpoint)
	if err != nil {
		return nil, err
	}

	caBytes, err := base64.StdEncoding.DecodeString(cluster.Status.CACert)
	if err != nil {
		return nil, err
	}

	return &rest.Config{
		Host:        u.Host,
		Prefix:      u.Path,
		BearerToken: cluster.Status.ServiceAccountToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caBytes,
		},
	}, nil
}

func (c *Manager) toServer(cluster *client.Cluster) (*record, error) {
	kubeConfig, err := c.toRESTConfig(cluster)
	if kubeConfig == nil || err != nil {
		return nil, err
	}

	clusterContext, err := config.NewClusterContext(c.ManagementConfig, *kubeConfig, cluster.ID)
	if err != nil {
		return nil, err
	}

	s := &record{}
	s.ctx, s.cancel = context.WithCancel(context.Background())

	s.handler, err = clusterapi.New(s.ctx, clusterContext)
	if err != nil {
		return nil, err
	}

	if err := clusterContext.Start(s.ctx); err != nil {
		return s, err
	}
	return s, nil
}
