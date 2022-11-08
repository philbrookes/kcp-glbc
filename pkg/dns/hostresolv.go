package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	gonet "net"
	"sync"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	log2 "github.com/kuadrant/kcp-glbc/pkg/_internal/log"
)

var (
	NoSuchHost = errors.New("no such host")
)

func IsNoSuchHostError(err error) bool {
	return err.Error() == NoSuchHost.Error()
}

type HostResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]HostAddress, error)
}

type HostAddress struct {
	Host string
	IP   gonet.IP
	TTL  time.Duration
	TXT  string
}

// ConfigMapHostResolver is a HostResolver that looks up the IP address of
// a host from a ConfigMap. Used for testing purposes
type ConfigMapHostResolver struct {
	Client kubernetes.Interface

	Name, Namespace string
}

var _ HostResolver = &ConfigMapHostResolver{}

func (r *ConfigMapHostResolver) LookupIPAddr(ctx context.Context, host string) ([]HostAddress, error) {
	configMap, err := r.Client.CoreV1().ConfigMaps(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	ipsValue, ok := configMap.Data[host]
	if !ok {
		return nil, &gonet.DNSError{Err: fmt.Sprintf("host %s not found in ConfigMap %s/%s", host, r.Name, r.Namespace)}
	}

	var ips []struct {
		IP  string
		TTL int
	}
	if err := json.Unmarshal([]byte(ipsValue), &ips); err != nil {
		return nil, err
	}

	result := make([]HostAddress, len(ips))
	for i, ip := range ips {
		result[i] = HostAddress{
			Host: host,
			IP:   gonet.ParseIP(ip.IP),
			TTL:  time.Duration(ip.TTL) * time.Second,
		}
	}

	return result, nil
}

func (r *ConfigMapHostResolver) TxtRecordExists(ctx context.Context, domain string, value string) (bool, error) {
	configMap, err := r.Client.CoreV1().ConfigMaps(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	if configMap.Data == nil {
		return false, NoSuchHost
	}

	ipsValue, ok := configMap.Data[domain]
	if !ok {
		return false, NoSuchHost
	}

	var values []struct {
		TXT string
	}

	if err := json.Unmarshal([]byte(ipsValue), &values); err != nil {
		return false, err
	}

	for _, entry := range values {
		log2.Logger.Info("checking for TXT record", "entry", entry, "value", value)
		if entry.TXT == value {
			return true, nil
		}
	}

	return false, nil
}

type DefaultHostResolver struct {
	Client dns.Client
}

func NewDefaultHostResolver() *DefaultHostResolver {
	return &DefaultHostResolver{
		Client: dns.Client{},
	}
}

func (hr *DefaultHostResolver) LookupIPAddr(ctx context.Context, host string) ([]HostAddress, error) {
	cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}

	for _, server := range cfg.Servers {
		m := dns.Msg{}
		m.SetQuestion(fmt.Sprintf("%s.", host), dns.TypeA)

		r, _, err := hr.Client.ExchangeContext(ctx, &m, fmt.Sprintf("%s:53", server))
		if err != nil {
			return nil, err
		}

		if len(r.Answer) == 0 {
			continue
		}

		results := make([]HostAddress, 0, len(r.Answer))
		for _, answer := range r.Answer {
			if a, ok := answer.(*dns.A); ok {
				results = append(results, HostAddress{
					Host: host,
					IP:   a.A,
					TTL:  time.Duration(a.Hdr.Ttl) * time.Second,
				})
			}
		}

		return results, nil
	}

	return nil, errors.New("no records found for host")
}

type SafeHostResolver struct {
	HostResolver

	mu sync.Mutex
}

func NewSafeHostResolver(inner HostResolver) *SafeHostResolver {
	return &SafeHostResolver{
		HostResolver: inner,
	}
}

func (r *SafeHostResolver) LookupIPAddr(ctx context.Context, host string) ([]HostAddress, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.HostResolver.LookupIPAddr(ctx, host)
}
