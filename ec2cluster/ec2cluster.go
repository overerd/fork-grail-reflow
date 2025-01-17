// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Package ec2cluster implements support for maintaining elastic
// clusters of Reflow instances on EC2.
//
// The EC2 instances created launch reflowlet agent processes that
// are given the user's profile token so that they can set up HTTPS
// servers that can perform mutual authentication to the reflow
// driver process and other reflowlets (for transferring objects) and
// also access external services like caching.
//
// The VM instances are configured to terminate if they are idle on
// EC2's billing hour boundary. They also terminate on any fatal
// reflowlet error.
package ec2cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	sa "github.com/grailbio/base/cloud/spotadvisor"
	"github.com/grailbio/base/limiter"
	"github.com/grailbio/base/status"
	"github.com/grailbio/base/sync/once"
	"github.com/grailbio/infra"
	"github.com/grailbio/infra/tls"
	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/ec2authenticator"
	"github.com/grailbio/reflow/ec2cluster/instances"
	"github.com/grailbio/reflow/errors"
	infra2 "github.com/grailbio/reflow/infra"
	"github.com/grailbio/reflow/internal/ecrauth"
	"github.com/grailbio/reflow/log"
	"github.com/grailbio/reflow/metrics"
	"github.com/grailbio/reflow/metrics/prometrics"
	"github.com/grailbio/reflow/pool"
	"github.com/grailbio/reflow/pool/client"
	"github.com/grailbio/reflow/taskdb"
	"golang.org/x/net/http2"
	"golang.org/x/time/rate"
)

func init() {
	infra.Register("ec2cluster", new(Cluster))
}

const (
	// allocAttemptInterval defines how often we attempt to allocate from existing pool
	// while waiting for an explicit allocation request to be completed.
	allocAttemptInterval       = 1 * time.Minute
	defaultClusterName         = "default"
	defaultMaxHourlyCostUSD    = 10.0
	defaultMaxPendingInstances = 5

	// unavailableInstanceTypeTtl is the ttl duration for which an instance type discovered
	// to be unavailable, remains so.
	unavailableInstanceTypeTtl = time.Hour

	// Cluster identification keys.  That is, the following are keys into Cluster.InstanceTags
	// which determine which set of instances belong to the "current" cluster.
	userKey        = "user"
	clusterNameKey = "cluster"
	managedByKey   = "managedby"
	versionKey     = "reflowlet:version"
)

// validateBootstrap is func for validating the bootstrap image
var validateBootstrap = defaultValidateBootstrap

// A Cluster implements a runner.Cluster backed by EC2.  The cluster expands
// with demand.  Instances are configured so that they shut down when they
// are idle on a billing boundary.
//
// No local state is stored; state is inferred from labels managed by EC2.
// Cluster supports safely sharing state across many processes. In
// this case, the processes coordinate to maintain a shared cluster,
// where instances can be used by any of the constituent processes.
// In the case of Reflow, this means that multiple runs (single or batch)
// share the same cluster efficiently.
type Cluster struct {
	pool.Mux `yaml:"-"`
	// HTTPClient is used to communicate to the reflowlet servers
	// running on the individual instances. In Cluster, this is done for
	// liveness/health checking.
	HTTPClient *http.Client `yaml:"-"`
	// Logger for cluster events.
	Log *log.Logger `yaml:"-"`
	// EC2 is the EC2 API instance through which EC2 calls are made.
	EC2 ec2iface.EC2API `yaml:"-"`
	// Authenticator authenticates the ECR repository that stores the
	// Reflowlet container.
	Authenticator ecrauth.Interface `yaml:"-"`
	// InstanceTags is the set of EC2 tags attached to instances created by this Cluster.
	InstanceTags map[string]string `yaml:"-"`
	// Labels is the set of labels that should be added as EC2 tags (for informational purpose only).
	Labels pool.Labels `yaml:"-"`
	// Spot is set to true when a spot instance is desired.
	Spot bool `yaml:"spot,omitempty"`
	// InstanceProfile is the EC2 instance profile to use for the cluster instances.
	InstanceProfile string `yaml:"instanceprofile,omitempty"`
	// SecurityGroup is the EC2 security group to use for cluster instances.
	SecurityGroup string `yaml:"securitygroup,omitempty"`
	// Subnets is the list of EC2 subnets ids based on which an appropriate subnet (for each AZ) will be determined.
	// That is, when Subnets is specified, the cluster will use ec2.DescribeSubnets API to determine AZ for each subnet.
	// When requesting a spot instance in a particular AZ, the appropriate subnet will be used.
	// If this list contains duplicate subnets for any AZ, behavior (of which subnet is used) is non-deterministic.
	Subnets []string `yaml:"subnets,omitempty"`
	// InstanceTypesMap stores the set of admissible instance types.
	// If nil, all instance types are permitted.
	InstanceTypesMap map[string]bool `yaml:"-"`
	// BootstrapImage is the URL of the image used for instance bootstrap.
	BootstrapImage string `yaml:"-"`
	// BootstrapExpiry is the maximum duration the bootstrap will wait for a reflowlet image after which it dies.
	BootstrapExpiry time.Duration `yaml:"-"`
	// ReflowVersion is the version of reflow binary compatible with this cluster.
	ReflowVersion string `yaml:"-"`
	// MaxPendingInstances is the maximum number of pending instances permitted.
	MaxPendingInstances int `yaml:"maxpendinginstances"`
	// MaxHourlyCostUSD is the maximum hourly cost of concurrent instances permitted (in USD).
	// A best effort is made to not go above this but races induced by multiple managers can increase the size
	// of the cluster beyond this limit. The limit is applied on maximum bid price and hence is an upper bound
	// on the actual incurred cost (which in practice would be much less).
	MaxHourlyCostUSD float64 `yaml:"maxhourlycostusd"`
	// DiskType is the EBS disk type to use.
	DiskType string `yaml:"disktype"`
	// DiskSpace is the number of GiB of disk space to allocate for each node.
	DiskSpace int `yaml:"diskspace"`
	// DiskSlices is the number of EBS volumes that are used. When DiskSlices > 1,
	// they are arranged in a RAID0 array to increase throughput.
	DiskSlices int `yaml:"diskslices"`
	// AMI is the VM image used to launch new instances.
	AMI string `yaml:"ami"`
	// Configuration for this Reflow instantiation. Used to provide configs to
	// EC2 instances.
	Configuration infra.Config `yaml:"-"`
	// AWS session
	Session *session.Session `yaml:"-"`
	// TaskDB implementation (if any) where rows are updated for newly created pools.
	TaskDB taskdb.TaskDB `yaml:"-"`

	// Public SSH keys.
	SshKeys []string `yaml:"sshkeys"`
	// AWS key name for launching instances.
	KeyName string `yaml:"keyname"`
	// Immortal determines whether instances should be made immortal.
	Immortal bool `yaml:"immortal,omitempty"`
	// NodeExporterMetricsPort determines whether to run a prometheus node_exporter daemon
	// on each Reflowlet. Setting a value runs the node_exporter daemon and configures it to
	// output prometheus metrics on the given port. Passing a non-zero value also adds an
	// additional route to the general Reflowlet server, such that metrics are made available
	// via proxy over the existing HTTPS connection and the following Reflow command:
	// $ reflow http https://${EC2_INST_PUBLIC_DNS}:9000/v1/node/metrics
	// If the user wishes to use other scrapers to fetch metrics from the Reflowlet over HTTP,
	// they may additionally choose to expose the port via the AWS settings for their Reflow
	// cluster.
	NodeExporterMetricsPort int `yaml:"nodeexportermetricsport,omitempty"`
	// CloudConfig is merged into the instance's cloudConfig before launching.
	CloudConfig cloudConfig `yaml:"cloudconfig"`
	// SpotProbeDepth is the probing depth for spot instance capacity checks.
	SpotProbeDepth int `yaml:"spotprobedepth,omitempty"`

	// Status is used to report cluster and instance status.
	Status *status.Group `yaml:"-"`

	// InstanceTypes defines the set of allowable EC2 instance types for
	// this cluster. If empty, all instance types are permitted.
	InstanceTypes []string `yaml:"instancetypes,omitempty"`
	// Name is the name of the cluster config, which defaults to defaultClusterName.
	// Multiple clusters can be launched/maintained simultaneously by using different names.
	Name string `yaml:"name,omitempty"`

	instanceState   *instanceState
	instanceConfigs map[string]instanceConfig

	mu    sync.Mutex
	pools map[string]reflowletPool

	// manager manages the cluster
	manager *Manager
	// spotProber probes for spot instance availability.
	spotProber *spotProber
	// descInstLimiter limits batch calls of `DescribeInstances`
	descInstLimiter *limiter.BatchLimiter
	// descSpotLimiter limits batch calls of `DescribeSpotInstanceRequests`
	descSpotLimiter *limiter.BatchLimiter
	// reqSpotLimiter limits calls of 'RequestSpotInstances'.
	reqSpotLimiter *rate.Limiter

	// refreshLimiter limits the rate of cluster refresh.
	refreshLimiter *rate.Limiter

	startOnce once.Task
	stats     *statsImpl
}

type header interface {
	Head(url string) (resp *http.Response, err error)
}

func defaultValidateBootstrap(burl string, h header) error {
	u, err := url.Parse(burl)
	if err != nil {
		return errors.E(errors.Fatal, "bootstrap image", err)
	}
	if u.Scheme != "https" {
		return errors.E(errors.Fatal, "bootstrap image", fmt.Errorf("scheme %s not supported: %s", u.Scheme, burl))
	}
	resp, err := h.Head(burl)
	switch {
	case err == nil && resp.StatusCode != http.StatusOK:
		err = errors.E(errors.Fatal, "bootstrap image", fmt.Errorf("HEAD %s: %s", burl, resp.Status))
	case resp == nil:
		err = errors.E(errors.Fatal, "bootstrap image", fmt.Errorf("HEAD %s: no response", burl))
	default:
		if contentType := resp.Header.Get("Content-Type"); contentType != "binary/octet-stream" {
			err = errors.E(errors.Fatal, "bootstrap image", fmt.Errorf("Content-Type not supported: %s", contentType))
		}
	}
	return err
}

// Help implements infra.Provider
func (*Cluster) Help() string {
	return "configure a cluster using AWS EC2 compute nodes"
}

// Config implements infra.Provider
func (c *Cluster) Config() interface{} {
	return c
}

// Init implements infra.Provider
func (c *Cluster) Init(tls tls.Certs, sess *session.Session, labels pool.Labels,
	bootstrapimage *infra2.BootstrapImage, reflowVersion *infra2.ReflowVersion, id *infra2.User, logger *log.Logger,
	ssh infra2.Ssh, mclient metrics.Client) error {
	c.Session = sess
	if c.Region() == "" {
		return errors.New("missing region parameter")
	}

	// If InstanceTypes are not defined, include built-in verified instance types.
	if len(c.InstanceTypes) == 0 {
		verified := instances.VerifiedByRegion[c.Region()]
		for _, typ := range instances.Types {
			if !verified[typ.Name].Attempted || verified[typ.Name].Verified {
				c.InstanceTypes = append(c.InstanceTypes, typ.Name)
			}
		}
		sort.Strings(c.InstanceTypes)
	}
	clientConfig, _, err := tls.HTTPS()
	if err != nil {
		return err
	}
	transport := &http.Transport{TLSClientConfig: clientConfig}
	http2.ConfigureTransport(transport)
	httpClient := &http.Client{Transport: transport}

	if reflowVersion.Value() == "" {
		return errors.New("no version specified in cluster configuration")
	}

	c.Authenticator = ec2authenticator.New(sess)
	c.HTTPClient = httpClient
	c.Log = logger.Tee(nil, "ec2cluster: ")
	if c.Name == "" {
		c.Name = defaultClusterName
	}
	c.Labels = labels.Copy()
	c.BootstrapImage = bootstrapimage.Value()
	c.ReflowVersion = string(*reflowVersion)
	c.SshKeys = ssh.Keys()
	if pclient, ok := mclient.(*prometrics.Client); ok {
		c.NodeExporterMetricsPort = pclient.NodeExporterPort
	}

	if c.MaxPendingInstances == 0 {
		c.MaxPendingInstances = defaultMaxPendingInstances
	}
	if c.MaxHourlyCostUSD == 0 {
		c.MaxHourlyCostUSD = defaultMaxHourlyCostUSD
	}

	if len(c.InstanceTypes) > 0 {
		c.InstanceTypesMap = make(map[string]bool)
		for _, typ := range c.InstanceTypes {
			c.InstanceTypesMap[typ] = true
		}
	}
	c.InstanceTags = make(map[string]string)
	c.InstanceTags["Name"] = fmt.Sprintf("%s (reflow)", id.User())
	c.InstanceTags[userKey] = id.User()
	c.InstanceTags[clusterNameKey] = c.Name
	c.InstanceTags[managedByKey] = "reflow"

	if c.DiskType == "" {
		return errors.New("missing disk type parameter")
	}
	if c.DiskSpace == 0 {
		return errors.New("missing disk space parameter")
	}
	if c.AMI, err = GetAMI(sess); err != nil {
		return err
	}
	if c.AMI == "" {
		return errors.New("missing AMI parameter")
	}
	if c.SecurityGroup == "" {
		return errors.New("missing EC2 security group")
	}

	// Construct the set of legal instances and set available disk space.
	var configs []instanceConfig
	c.instanceConfigs = make(map[string]instanceConfig)
	for _, config := range instanceTypes {
		config.Resources["disk"] = float64(c.DiskSpace << 30)
		if c.InstanceTypesMap == nil || c.InstanceTypesMap[config.Type] {
			configs = append(configs, config)
		}
		c.instanceConfigs[config.Type] = config
	}
	for inst := range c.InstanceTypesMap {
		if _, ok := instanceTypes[inst]; !ok {
			c.Log.Debugf("instance type unknown: %v", inst)
		}
	}
	if len(configs) == 0 {
		return errors.New("no configured instance types")
	}
	adv, _ := sa.NewSpotAdvisor(c.Log, context.Background().Done())
	c.instanceState = newInstanceState(configs, unavailableInstanceTypeTtl, c.Region(), adv)
	c.manager = NewManager(c, c.MaxHourlyCostUSD, c.MaxPendingInstances, c.Log)
	c.spotProber = NewSpotProber(
		func(ctx context.Context, instanceType string, depth int) (bool, error) {
			return ec2HasCapacity(ctx, c.EC2, c.AMI, instanceType, depth, c.Log)
		},
		c.SpotProbeDepth, 1*time.Minute)
	c.pools = make(map[string]reflowletPool)
	c.stats = newStats()
	return nil
}

// ExportStats exports the cluster stats to expvar.
func (c *Cluster) ExportStats() {
	c.stats.publish()
}

// Verify verifies configuration settings.
func (c *Cluster) Verify() error {
	err := validateBootstrap(c.BootstrapImage, http.DefaultClient)
	if err != nil {
		err = errors.E(errors.Fatal, fmt.Sprintf("bootstrap image: %s", c.BootstrapImage), err)
	}
	return err
}

// initialize initializes the cluster by starting maintenance goroutines.
func (c *Cluster) initialize(ctx context.Context, wg *sync.WaitGroup) {
	if c.BootstrapExpiry == 0 {
		var rc *infra2.ReflowletConfig
		if rcerr := c.Configuration.Instance(&rc); rcerr == nil {
			c.BootstrapExpiry = rc.MaxIdleDuration
		}
	}
	if err := c.Configuration.Instance(&c.TaskDB); err != nil {
		c.Log.Debugf("cluster taskdb: %v", err)
	}
	c.EC2 = ec2.New(c.Session, &aws.Config{MaxRetries: aws.Int(13)})
	if len(c.Subnets) > 0 {
		if err := computeAzSubnetMap(c.EC2, c.Subnets, c.Log); err != nil {
			c.Log.Error(err)
		}
	}
	c.descInstLimiter = limiter.NewBatchLimiter(
		&descInstBatchApi{api: c.EC2, log: c.Log, maxPerBatch: 100},
		/* 5 qps */ rate.NewLimiter(rate.Every(time.Second), 5))
	c.descSpotLimiter = limiter.NewBatchLimiter(
		&descSpotBatchApi{api: c.EC2, log: c.Log, maxPerBatch: 30},
		/* 2 qps */ rate.NewLimiter(rate.Every(time.Second), 2))
	c.reqSpotLimiter = rate.NewLimiter(rate.Every(time.Second), 5) // 5 qps
	c.refreshLimiter = rate.NewLimiter(rate.Every(time.Second), 1) // 1 qps
	c.SetCaching(true)
	c.manager.Start(ctx, wg)
}

// Region is the AWS region to use for launching new EC2 instances.
func (c *Cluster) Region() string {
	if c.Session == nil {
		panic("ec2cluster Session must be set")
	}
	return aws.StringValue(c.Session.Config.Region)
}

// CanAllocate returns whether this cluster can allocate the given amount of resources.
func (c *Cluster) CanAllocate(r reflow.Resources) (bool, error) {
	if !c.instanceState.Available(r) {
		max := c.instanceState.Largest()
		return false, errors.E(errors.ResourcesExhausted,
			errors.Errorf("requested resources %s not satisfiable even by largest available instance type %s with resources %s", r, max.Type, max.Resources))
	}
	return true, nil
}

// Allocate reserves an alloc with within the resource requirement
// boundaries form this cluster. If an existing instance can serve
// the request, it is returned immediately; otherwise new instance(s)
// are spun up to handle the allocation.
func (c *Cluster) Allocate(ctx context.Context, req reflow.Requirements, labels pool.Labels) (alloc pool.Alloc, err error) {
	if !c.startOnce.Done() {
		panic("uninitialized ec2cluster - Start() not called ?")
	}
	if ok, er := c.CanAllocate(req.Min); !ok {
		return nil, er
	}
	c.Log.Debugf("allocate %s", req)
	const allocTimeout = 30 * time.Second
	if c.Size() > 0 {
		c.Log.Debug("attempting to allocate from existing pool")
		actx, acancel := context.WithTimeout(ctx, allocTimeout)
		alloc, err := pool.Allocate(actx, c, req, labels)
		acancel()
		if err == nil {
			return alloc, nil
		}
		c.Log.Debugf("failed to allocate from existing pool: %v; provisioning from EC2", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ticker := time.NewTicker(allocAttemptInterval)
	defer ticker.Stop()
	needch := c.manager.Allocate(ctx, req)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-needch:
			actx, acancel := context.WithTimeout(ctx, allocTimeout)
			alloc, err := pool.Allocate(actx, c, req, labels)
			acancel()
			if err == nil {
				return alloc, nil
			}
			if err = ctx.Err(); err != nil {
				return alloc, err // don't try to allocate again if ctx is canceled
			}
			c.Log.Errorf("failed to allocate from pool: %v; provisioning new instances", err)
			// We didn't get it--try again!
			needch = c.manager.Allocate(ctx, req)
		case <-ticker.C:
			actx, acancel := context.WithTimeout(ctx, allocTimeout)
			alloc, err := pool.Allocate(actx, c, req, labels)
			acancel()
			if err == nil {
				return alloc, nil
			}
		}
	}
}

// Start initializes the cluster and it should be called before any `pool.Pool` operations are performed on the cluster.
// Start uses the provided context to maintain the cluster and upon cancellation, the cluster will shutdown.
// Start can be called multiple times, but only parameters passed to the first call (which started the cluster)
// are relevant.
// Start takes a WaitGroup whose counter is updated to reflect background goroutines.
func (c *Cluster) Start(ctx context.Context, wg *sync.WaitGroup) {
	_ = c.startOnce.Do(func() error {
		if err := c.Verify(); err != nil {
			// panic if Verify() hasn't already been called (and if it throws an error).
			panic(fmt.Sprintf("unexpected ec2cluster.Verify: %v", err))
		}
		c.initialize(ctx, wg)
		return nil
	})
}

// QueryTags returns the list of tags to use to query for instances belonging to this cluster.
// This includes all InstanceTags that are set on any instance brought up by this cluster,
// and a "reflowlet:version" tag (set on the instance by the reflowlet once it comes up)
// to match the ReflowVersion of this cluster.
func (c *Cluster) QueryTags() map[string]string {
	qtags := make(map[string]string)
	for k, v := range c.InstanceTags {
		qtags[k] = v
	}
	qtags[versionKey] = c.ReflowVersion
	return qtags
}

// Probe attempts to instantiate an EC2 instance of the given type and returns
// the available resources on it (as per its offers), a duration and an error.
// In case of a nil error:
// - the duration represents how long it took for a usable Reflowlet to come up on that instance type.
// - the resources represents how much actual resources are available/usable on that instance type.
// Note: Of course the above are based on a single data point.
// A non-nil error means that the reflowlet failed to come up on this instance type.
// The error could be due to context deadline, in case we gave up waiting for it to come up.
func (c *Cluster) Probe(ctx context.Context, instanceType string) (reflow.Resources, time.Duration, error) {
	if !c.startOnce.Done() {
		panic("uninitialized ec2cluster - Start() not called ?")
	}
	var r reflow.Resources
	config := c.instanceConfigs[instanceType]
	i := c.newInstance(config)
probe:
	i.Task = c.Status.Startf("%s", instanceType)
	i.Go(context.Background())
	if i.err != nil {
		// If the error was due to Spot unavailability, try on-demand instead.
		if i.Spot && errors.Is(errors.Unavailable, i.err) {
			i.Task.Printf("spot unavailable, trying on-demand")
			i.Spot = false
			i.err = nil
			i.Task.Done()
			goto probe
		}
		i.Task.Printf("%v", i.err.Error())
	}
	if i.ec2inst != nil {
		// Terminate the instance before exiting, if one does exist,
		// whether we ultimately ended up with an error or not
		defer ec2TerminateInstance(i.EC2, *i.ec2inst.InstanceId, i.Log)
	}
	if i.err == nil {
		iid, dns := *i.ec2inst.InstanceId, *i.ec2inst.PublicDnsName
		baseurl := fmt.Sprintf("https://%s:9000/v1/", dns)
		if clnt, err := client.New(baseurl, c.HTTPClient, nil); err != nil {
			c.Log.Errorf("client %s: %v", baseurl, err)
		} else {
			c.Log.Debugf("discovered instance %s (%s) %s", iid, instanceType, dns)
			offers, oerr := clnt.Offers(ctx)
			switch {
			case oerr != nil:
				c.Log.Errorf("client %s offers: %v", baseurl, oerr)
			case len(offers) != 1:
				c.Log.Errorf("client %s offers: expected 1 offer, got %d: %v", baseurl, len(offers), offers)
			default:
				r = offers[0].Available()
			}
		}

	}
	i.Task.Done()
	dur := i.Task.Value().End.Sub(i.Task.Value().Begin)
	return r, dur.Round(time.Second), i.err
}

func (c *Cluster) newInstance(config instanceConfig) *instance {
	return &instance{
		HTTPClient:              c.HTTPClient,
		ReflowConfig:            c.Configuration,
		Config:                  config,
		Log:                     c.Log,
		Authenticator:           c.Authenticator,
		EC2:                     c.EC2,
		TaskDB:                  c.TaskDB,
		InstanceTags:            c.InstanceTags,
		Labels:                  c.Labels,
		Spot:                    c.Spot,
		InstanceProfile:         c.InstanceProfile,
		SecurityGroup:           c.SecurityGroup,
		Region:                  c.Region(),
		BootstrapImage:          c.BootstrapImage,
		BootstrapExpiry:         c.BootstrapExpiry,
		Price:                   config.Price[c.Region()],
		EBSType:                 c.DiskType,
		EBSSize:                 uint64(config.Resources["disk"]) >> 30,
		NEBS:                    c.DiskSlices,
		AMI:                     c.AMI,
		SshKeys:                 c.SshKeys,
		KeyName:                 c.KeyName,
		SpotProber:              c.spotProber,
		DescInstLimiter:         c.descInstLimiter,
		DescSpotLimiter:         c.descSpotLimiter,
		ReqSpotLimiter:          c.reqSpotLimiter,
		Immortal:                c.Immortal,
		NodeExporterMetricsPort: c.NodeExporterMetricsPort,
		CloudConfig:             c.CloudConfig,
		ReflowVersion:           c.ReflowVersion,
	}
}

// Available returns the cheapest available instance specification that
// has at least the required resources.
func (c *Cluster) Available(need reflow.Resources, maxPrice float64) (InstanceSpec, bool) {
	config, ok := c.instanceState.MinAvailable(need, c.Spot, maxPrice)
	return InstanceSpec{config.Type, config.Resources}, ok
}

// Launch launches an EC2 instance based on the given spec and returns a ManagedInstance.
func (c *Cluster) Launch(ctx context.Context, spec InstanceSpec) ManagedInstance {
	config, ok := c.instanceConfigs[spec.Type]
	if !ok {
		return spec.Instance("")
	}
	i := c.newInstance(config)
	i.Task = c.Status.Startf("%s", spec.Type)
	i.Go(ctx)
	i.Task.Done()
	switch {
	case i.err == nil:
	case errors.Is(errors.Unavailable, i.err):
		c.Log.Debugf("instance type %s unavailable in region %s: %v", i.Config.Type, c.Region(), i.err)
		c.instanceState.Unavailable(i.Config)
		fallthrough
	// TODO(swami): Deal with Fatal errors appropriately by propagating them up the stack.
	// In case of Fatal errors, retrying is going to result in the same error, so its better
	// to just escalate up the stack and stop trying.
	// case errors.Is(errors.Fatal, inst.Err()):
	default:
	}
	return i.ManagedInstance()
}

func (c *Cluster) Notify(waiting, pending reflow.Resources) {
	c.printState(fmt.Sprintf("waiting%s, pending%s", waiting, pending))
}

func (c *Cluster) Refresh(ctx context.Context) (map[string]string, error) {
	state, err := c.getEC2State(ctx)
	if err != nil {
		return nil, err
	}
	defer c.printState("")
	c.mu.Lock()
	defer c.mu.Unlock()
	// Remove from pool instances that are not available on EC2.
	for id := range c.pools {
		if _, ok := state[id]; !ok {
			delete(c.pools, id)
		}
	}
	// Add instances on EC2 that are not in the pool.
	for id, inst := range state {
		if _, ok := c.pools[id]; !ok {
			iid, typ, dns := *inst.InstanceId, *inst.InstanceType, *inst.PublicDnsName
			baseurl := fmt.Sprintf("https://%s:9000/v1/", dns)
			clnt, cerr := client.New(baseurl, c.HTTPClient, nil)
			if cerr != nil {
				c.Log.Errorf("client %s: %v", baseurl, cerr)
				continue
			}
			c.Log.Debugf("discovered instance %s (%s) %s", iid, typ, dns)
			// Add instance to the pool.
			c.pools[iid] = reflowletPool{inst, clnt}
		}
	}
	c.stats.setInstancesStats(state)
	c.SetPools(vals(c.pools))
	m := make(map[string]string, len(c.pools))
	for iid, reflowlet := range c.pools {
		m[iid] = aws.StringValue(reflowlet.inst.InstanceType)
	}
	return m, err
}

// getEC2State gets the current state of the cluster by querying EC2.
// The cluster consists of all EC2 instances returned by AWS (at that moment)
// which have the set of tags returned by `QueryTags`.
// At the time of writing this, its unclear how much (if any) propagation delay
// exists between tagging an instance and the instance being returned by the AWS API.
func (c *Cluster) getEC2State(ctx context.Context) (map[string]*reflowletInstance, error) {
	var filters []*ec2.Filter
	for k, v := range c.QueryTags() {
		filters = append(filters, &ec2.Filter{
			Name: aws.String("tag:" + k), Values: []*string{aws.String(v)},
		})
	}
	req := &ec2.DescribeInstancesInput{Filters: filters, MaxResults: aws.Int64(1000)}
	state := make(map[string]*reflowletInstance)
	for req != nil {
		ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.refreshLimiter.Wait(ctx2)
		if err != nil {
			cancel()
			return nil, errors.E("refreshLimiter", errors.Temporary, err)
		}
		resp, err := c.EC2.DescribeInstancesWithContext(ctx2, req)
		cancel()
		if err != nil {
			return nil, err
		}
		for _, resv := range resp.Reservations {
			for _, inst := range resv.Instances {
				switch *inst.State.Name {
				case ec2.InstanceStateNameRunning:
					state[*inst.InstanceId] = newReflowletInstance(inst)
				default:
				}
			}
		}
		if resp.NextToken != nil {
			req.NextToken = resp.NextToken
		} else {
			req = nil
		}
	}
	return state, nil
}

func (c *Cluster) InstancePriceUSD(typ string) float64 {
	config := c.instanceConfigs[typ]
	return config.Price[c.Region()]
}

func (c *Cluster) CheapestInstancePriceUSD() float64 {
	return c.InstancePriceUSD(c.instanceState.Cheapest().Type)
}

// GetName implements runner.Cluster
func (c *Cluster) GetName() string { return c.Name }

func (c *Cluster) printState(suffix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var (
		counts     []string
		totalPrice float64
		total      reflow.Resources
	)
	n := 0
	instanceTypeCounts := instTypes(c.pools)
	for typ, ntyp := range instanceTypeCounts {
		counts = append(counts, fmt.Sprintf("%s:%d", typ, ntyp))
		config := c.instanceConfigs[typ]
		var r reflow.Resources
		r.Scale(config.Resources, float64(ntyp))
		total.Add(total, r)
		totalPrice += config.Price[c.Region()] * float64(ntyp)
		n += ntyp
	}
	sort.Strings(counts)
	msg := fmt.Sprintf("%d instances: %s (<=$%.1f/hr), total%s", n, strings.Join(counts, ","), totalPrice, total)
	if suffix != "" {
		msg = fmt.Sprintf("%s, %s", msg, suffix)
	}
	c.Status.Print(msg)
	c.Log.Debug(msg)
}

type reflowletPool struct {
	inst *reflowletInstance
	pool pool.Pool
}

func vals(m map[string]reflowletPool) []pool.Pool {
	pools := make([]pool.Pool, len(m))
	i := 0
	for _, p := range m {
		pools[i] = p.pool
		i++
	}
	return pools
}

func instTypes(pools map[string]reflowletPool) map[string]int {
	instanceTypeCounts := make(map[string]int)
	for _, instance := range pools {
		instanceTypeCounts[*instance.inst.InstanceType]++
	}
	return instanceTypeCounts
}
