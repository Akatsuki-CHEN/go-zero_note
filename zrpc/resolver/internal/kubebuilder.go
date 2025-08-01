//go:build !no_k8s

package internal

import (
	"context"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/threading"
	"github.com/zeromicro/go-zero/zrpc/resolver/internal/kube"
	"google.golang.org/grpc/resolver"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	resyncInterval = 5 * time.Minute
	nameSelector   = "metadata.name="
)

type kubeResolver struct {
	cc     resolver.ClientConn
	inf    informers.SharedInformerFactory
	stopCh chan struct{}
}

func (r *kubeResolver) Close() {
	close(r.stopCh)
}

func (r *kubeResolver) ResolveNow(_ resolver.ResolveNowOptions) {}

func (r *kubeResolver) start() {
	threading.GoSafe(func() {
		r.inf.Start(r.stopCh)
	})
}

type kubeBuilder struct{}

func (b *kubeBuilder) Build(target resolver.Target, cc resolver.ClientConn,
	_ resolver.BuildOptions) (resolver.Resolver, error) {
	// 解析k8s的target
	svc, err := kube.ParseTarget(target)
	if err != nil {
		return nil, err
	}

	// 获取k8s的集群配置
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	// 创建k8s的客户端
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	// 当服务端口未指定时，从 Kubernetes Endpoints 中获取实际的服务端口信息
	if svc.Port == 0 {
		// getting endpoints is only to get the port
		endpoints, err := cs.CoreV1().Endpoints(svc.Namespace).Get(
			context.Background(), svc.Name, v1.GetOptions{})
		if err != nil {
			return nil, err
		}

		svc.Port = int(endpoints.Subsets[0].Ports[0].Port)
	}

	handler := kube.NewEventHandler(
		// 这个方法是：将新的服务器地址列表更新到 gRPC 客户端，这样客户端就知道可以连接到哪些服务器实例
		func(endpoints []string) {
			/* 优化：
			在大多数情况下，客户端并不需要知道所有的服务实例：
			gRPC 客户端通常只需要少数几个健康的实例即可
			负载均衡算法可以在较小的子集中实现良好的负载分布
			通过随机打乱后选择前 32 个实例（subsetSize = 32），可以确保选择的实例具有随机性和代表性
			*/
			endpoints = subset(endpoints, subsetSize)
			addrs := make([]resolver.Address, 0, len(endpoints))
			for _, val := range endpoints {
				addrs = append(addrs, resolver.Address{
					Addr: fmt.Sprintf("%s:%d", val, svc.Port),
				})
			}

			if err := cc.UpdateState(resolver.State{
				Addresses: addrs,
			}); err != nil {
				logx.Error(err)
			}
		},
	)

	// 用于监听 Kubernetes 中的 Endpoints 资源变化
	inf := informers.NewSharedInformerFactoryWithOptions(cs, resyncInterval,
		informers.WithNamespace(svc.Namespace),
		informers.WithTweakListOptions(func(options *v1.ListOptions) {
			options.FieldSelector = nameSelector + svc.Name
		}))
	in := inf.Core().V1().Endpoints()
	_, err = in.Informer().AddEventHandler(handler)
	if err != nil {
		return nil, err
	}

	// 这里拿到的是Pod的地址而不是 Service 的 ClusterIP
	// get the initial endpoints, cannot use the previous endpoints,
	// because the endpoints may be updated before/after the informer is started.
	endpoints, err := cs.CoreV1().Endpoints(svc.Namespace).Get(
		context.Background(), svc.Name, v1.GetOptions{})
	if err != nil {
		return nil, err
	}

	handler.Update(endpoints)

	r := &kubeResolver{
		cc:     cc,
		inf:    inf,
		stopCh: make(chan struct{}),
	}
	r.start()

	return r, nil
}

func (b *kubeBuilder) Scheme() string {
	return KubernetesScheme
}
