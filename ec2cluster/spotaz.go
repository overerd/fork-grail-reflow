package ec2cluster

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/grailbio/base/sync/once"
	"github.com/grailbio/reflow/log"
)

var (
	azIdToNameOnce once.Task
	// azIdToName maps Availability Zone ids to names.
	// See https://docs.aws.amazon.com/ram/latest/userguide/working-with-az-ids.html
	// The mapping is different for each AWS account, but does not change over time.
	azIdToName = make(map[string]string)

	azNameToSubnetOnce once.Task
	// azNameToSubnet maps Availability Zone names to subnets.
	azNameToSubnet = make(map[string]string)
)

// availabilityZones returns a list of availability zone names for the given region.
func availabilityZones(api ec2iface.EC2API, region string) ([]string, error) {
	err := azIdToNameOnce.Do(func() error {
		req := &ec2.DescribeAvailabilityZonesInput{
			Filters: []*ec2.Filter{{
				Name:   aws.String("region-name"),
				Values: aws.StringSlice([]string{region}),
			}},
		}
		resp, err := api.DescribeAvailabilityZones(req)
		if err != nil {
			return err
		}
		for _, az := range resp.AvailabilityZones {
			azIdToName[*az.ZoneId] = *az.ZoneName
		}
		return nil
	})
	zones := make([]string, 0, len(azIdToName))
	for _, zone := range azIdToName {
		zones = append(zones, zone)
	}
	return zones, err
}

// subnetsByVpc computes a mapping of availability-zone name to subnet id based on the given VPC Id.
func subnetsByVpc(api ec2iface.EC2API, vpcid string, log *log.Logger) error {
	return azNameToSubnetOnce.Do(func() error {
		req := &ec2.DescribeSubnetsInput{Filters: []*ec2.Filter{{
			Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcid})}}}
		resp, err := api.DescribeSubnets(req)
		if err != nil {
			return err
		}
		subnetsByAz := make(map[string][]string)
		for _, sn := range resp.Subnets {
			if sn.AvailabilityZone == nil || sn.SubnetId == nil {
				continue
			}
			var (
				az       = *sn.AvailabilityZone
				subnetId = *sn.SubnetId
				subnets  []string
				ok       bool
			)
			if subnets, ok = subnetsByAz[az]; !ok {
				subnets = []string{}
			}
			subnetsByAz[az] = append(subnets, subnetId)
		}
		for az, subnets := range subnetsByAz {
			if len(subnets) != 1 {
				log.Debugf("VPC Id: %s ambiguous subnets for AZ %s: %s",
					vpcid, az, strings.Join(subnets, ","))
				continue
			}
			log.Debugf("VPC Id: %s based AZ to subnet mapping %s -> %s", vpcid, az, subnets[0])
			azNameToSubnet[az] = subnets[0]
		}
		return nil
	})
}

// azForSubnetId returns availability-zone name for the given subnet id.
func azForSubnetId(api ec2iface.EC2API, subnetId string) (string, error) {
	resp, err := api.DescribeSubnets(&ec2.DescribeSubnetsInput{SubnetIds: aws.StringSlice([]string{subnetId})})
	var az string
	switch {
	case err != nil:
	case len(resp.Subnets) != 1:
		err = fmt.Errorf("did not yield exactly one result (got %d)", len(resp.Subnets))
	default:
		az = *resp.Subnets[0].AvailabilityZone
	}
	return az, err
}

// subnetForAZ returns an appropriate subnet for the given availability-zone name.
// subnetForAZ must be called only subnetsByVpc has been called by the same process earlier,
// otherwise, it will always return empty strings.
func subnetForAZ(azName string) string {
	return azNameToSubnet[azName]
}
