package ec2cluster

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	sa "github.com/grailbio/base/cloud/spotadvisor"
	"github.com/grailbio/reflow"
)

// use a max price higher than the most expensive instance type
const testMaxPrice = 100

func TestInstanceState(t *testing.T) {
	var instances []instanceConfig
	for _, config := range instanceTypes {
		config.Resources["disk"] = float64(2000 << 30)
		instances = append(instances, config)
	}
	is := newInstanceState(instances, 1*time.Second, "us-west-2", nil)
	for _, tc := range []struct {
		r                reflow.Resources
		wantMin, wantMax string
	}{
		{reflow.Resources{"mem": 2 << 30, "cpu": 1, "disk": 10 << 30}, "t3a.medium", "x1e.32xlarge"},
		{reflow.Resources{"mem": 10 << 30, "cpu": 5, "disk": 100 << 30}, "c5.2xlarge", "x1e.32xlarge"},
		{reflow.Resources{"mem": 30 << 30, "cpu": 8, "disk": 800 << 30}, "r5.2xlarge", "x1e.32xlarge"},
		{reflow.Resources{"mem": 30 << 30, "cpu": 16, "disk": 800 << 30}, "m5.4xlarge", "x1e.32xlarge"},
		{reflow.Resources{"mem": 60 << 30, "cpu": 16, "disk": 400 << 30}, "r5.4xlarge", "x1e.32xlarge"},
		{reflow.Resources{"mem": 122 << 30, "cpu": 16, "disk": 400 << 30}, "r5a.8xlarge", "x1e.32xlarge"},
		{reflow.Resources{"mem": 60 << 30, "cpu": 32, "disk": 1000 << 30}, "c5.9xlarge", "x1e.32xlarge"},
		{reflow.Resources{"mem": 120 << 30, "cpu": 32, "disk": 2000 << 30}, "r5a.8xlarge", "x1e.32xlarge"},
	} {
		for _, spot := range []bool{true, false} {
			if got, _ := is.MinAvailable(tc.r, spot, testMaxPrice); got.Type != tc.wantMin {
				t.Errorf("got %v, want %v for spot %v, resources %v", got.Type, tc.wantMin, spot, tc.r)
			}
			if got, _ := is.MaxAvailable(tc.r, spot); got.Type != tc.wantMax {
				t.Errorf("got %v, want %v for spot %v, resources %v", got.Type, tc.wantMax, spot, tc.r)
			}
		}
	}
}

func TestInstanceStateLargest(t *testing.T) {
	instances := newInstanceState(
		[]instanceConfig{instanceTypes["c5.2xlarge"]},
		1*time.Second, "us-west-2", nil)
	if got, want := instances.Largest().Type, "c5.2xlarge"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	instances = newInstanceState(
		[]instanceConfig{instanceTypes["c5.2xlarge"], instanceTypes["c5.9xlarge"]},
		1*time.Second, "us-west-2", nil)
	if got, want := instances.Largest().Type, "c5.9xlarge"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	instances = newInstanceState(
		[]instanceConfig{instanceTypes["r5a.8xlarge"], instanceTypes["c5.9xlarge"]},
		1*time.Second, "us-west-2", nil)
	if got, want := instances.Largest().Type, "r5a.8xlarge"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestInstanceStateCheapest(t *testing.T) {
	instances := newInstanceState(
		[]instanceConfig{instanceTypes["c5.2xlarge"]},
		1*time.Second, "us-west-2", nil)
	if got, want := instances.Cheapest().Type, "c5.2xlarge"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	instances = newInstanceState(
		[]instanceConfig{instanceTypes["c5.2xlarge"], instanceTypes["c5.9xlarge"]},
		1*time.Second, "us-west-2", nil)
	if got, want := instances.Cheapest().Type, "c5.2xlarge"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	instances = newInstanceState(
		[]instanceConfig{instanceTypes["r5a.8xlarge"], instanceTypes["c5.9xlarge"]},
		1*time.Second, "us-west-2", nil)
	if got, want := instances.Cheapest().Type, "c5.9xlarge"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestInstanceStateUnavailable(t *testing.T) {
	const sleepTime = 200 * time.Millisecond
	instances := newInstanceState(
		[]instanceConfig{instanceTypes["c5.2xlarge"]},
		sleepTime, "us-west-2", nil)
	cfg, _ := instances.Type("c5.2xlarge")
	gotCfg, gotAvail := instances.MinAvailable(reflow.Resources{"mem": 2 << 30, "cpu": 1}, true, 100.0)
	if wantCfg, wantAvail := cfg, true; !reflect.DeepEqual(gotCfg, wantCfg) || gotAvail != wantAvail {
		t.Errorf("Instance type: got %v, want %v, available: got %v, want %v ", gotCfg, wantCfg, gotAvail, wantAvail)
	}
	instances.Unavailable(cfg)
	zeroCfg := instanceConfig{}
	gotCfg, gotAvail = instances.MinAvailable(reflow.Resources{"mem": 2 << 30, "cpu": 1}, true, 100.0)
	if wantCfg, wantAvail := zeroCfg, false; !reflect.DeepEqual(gotCfg, wantCfg) || gotAvail != wantAvail {
		t.Errorf("Instance type: got %v, want %v, available: got %v, want %v ", gotCfg, wantCfg, gotAvail, wantAvail)
	}
	time.Sleep(sleepTime)
	gotCfg, gotAvail = instances.MinAvailable(reflow.Resources{"mem": 2 << 30, "cpu": 1}, true, 100.0)
	if wantCfg, wantAvail := cfg, true; !reflect.DeepEqual(gotCfg, wantCfg) || gotAvail != wantAvail {
		t.Errorf("Instance type: got %v, want %v, available: got %v, want %v ", gotCfg, wantCfg, gotAvail, wantAvail)
	}
}

func TestInstanceStateWithAdvisor(t *testing.T) {
	var instances []instanceConfig
	testAdvisorAllHighInterrupt := testAdvisor{}
	for _, config := range instanceTypes {
		config.Resources["disk"] = float64(2000 << 30)
		instances = append(instances, config)
		testAdvisorAllHighInterrupt[sa.InstanceType(config.Type)] = sa.LessThanTwentyPct
	}
	for _, tc := range []struct {
		name             string
		r                reflow.Resources
		adv              testAdvisor
		spot             bool
		wantMin, wantMax string
	}{
		{
			"first choice doesn't satisfy interrupt prob threshold",
			reflow.Resources{"mem": 2 << 30, "cpu": 1, "disk": 10 << 30},
			map[sa.InstanceType]sa.InterruptProbability{
				"t3a.medium":   sa.LessThanTwentyPct, // min candidate, but above the threshold
				"t3.medium":    sa.LessThanTenPct,    // min candidate, within the threshold -> wantMin
				"x1e.32xlarge": sa.Any,               // max candidate, but above the threshold
				"x1.32xlarge":  sa.LessThanFivePct,   // max candidate, within the threshold -> wantMax
			},
			true,
			"t3.medium",
			"x1.32xlarge",
		},
		{
			"ignore interrupt probability when spot=false",
			reflow.Resources{"mem": 2 << 30, "cpu": 1, "disk": 10 << 30},
			map[sa.InstanceType]sa.InterruptProbability{
				"t3a.medium":   sa.LessThanTwentyPct, // min candidate, but above the threshold
				"x1e.32xlarge": sa.Any,               // max candidate, but above the threshold
			},
			false, // since spot is false, these instances should be picked despite not satisfying the threshold
			"t3a.medium",
			"x1e.32xlarge",
		},
		{
			"fall back to higher interrupt probability threshold",
			reflow.Resources{"mem": 2 << 30, "cpu": 1, "disk": 10 << 30},
			testAdvisorAllHighInterrupt, // none of the instance types meet the initial 10% threshold
			true,
			"t3a.medium", // we should still pick the same instances as we try higher thresholds
			"x1e.32xlarge",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// create an instanceState using the testcase's advisor
			is := newInstanceState(instances, 1*time.Second, "us-west-2", tc.adv)

			if got, _ := is.MinAvailable(tc.r, tc.spot, testMaxPrice); got.Type != tc.wantMin {
				t.Errorf("got %v, want %v for spot %v, resources %v", got.Type, tc.wantMin, tc.spot, tc.r)
			}
			if got, _ := is.MaxAvailable(tc.r, tc.spot); got.Type != tc.wantMax {
				t.Errorf("got %v, want %v for spot %v, resources %v", got.Type, tc.wantMax, tc.spot, tc.r)
			}
		})
	}
}

// testAdvisor implements ec2cluster.advisor.
type testAdvisor map[sa.InstanceType]sa.InterruptProbability

func (a testAdvisor) GetMaxInterruptProbability(_ sa.OsType, _ sa.AwsRegion, it sa.InstanceType) (sa.InterruptProbability, error) {
	ip, ok := a[it]
	if !ok {
		return -1, fmt.Errorf("testAdvisor does not have an entry for %s", it)
	}
	return ip, nil
}
