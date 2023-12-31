package aws

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/go-logr/logr"
	"github.com/redhat-appstudio/multi-platform-controller/pkg/cloud"
	v1 "k8s.io/api/core/v1"
	types2 "k8s.io/apimachinery/pkg/types"
	"net"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
)

const MultiPlatformManaged = "MultiPlatformManaged"

func Ec2Provider(platformName string, config map[string]string, systemNamespace string) cloud.CloudProvider {
	disk, err := strconv.Atoi(config["dynamic."+platformName+".disk"])
	if err != nil {
		disk = 40
	}
	return AwsDynamicConfig{Region: config["dynamic."+platformName+".region"],
		Ami:             config["dynamic."+platformName+".ami"],
		InstanceType:    config["dynamic."+platformName+".instance-type"],
		KeyName:         config["dynamic."+platformName+".key-name"],
		Secret:          config["dynamic."+platformName+".aws-secret"],
		SecurityGroup:   config["dynamic."+platformName+".security-group"],
		SystemNamespace: systemNamespace,
		Disk:            int32(disk),
	}
}

func (configMapInfo AwsDynamicConfig) LaunchInstance(kubeClient client.Client, log *logr.Logger, ctx context.Context, name string, instanceTag string) (cloud.InstanceIdentifier, error) {
	log.Info(fmt.Sprintf("attempting to launch AWS instance for %s", name))
	// Load AWS credentials and configuration

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(SecretCredentialsProvider{Name: configMapInfo.Secret, Namespace: "multi-platform-controller", Client: kubeClient}),
		config.WithRegion(configMapInfo.Region))
	if err != nil {
		return "", err
	}

	// Create an EC2 client
	ec2Client := ec2.NewFromConfig(cfg)

	// Specify the parameters for the new EC2 instance
	launchInput := &ec2.RunInstancesInput{
		KeyName:        aws.String(configMapInfo.KeyName),
		ImageId:        aws.String(configMapInfo.Ami), //ARM RHEL
		InstanceType:   types.InstanceType(configMapInfo.InstanceType),
		MinCount:       aws.Int32(1),
		MaxCount:       aws.Int32(1),
		EbsOptimized:   aws.Bool(true),
		SecurityGroups: []string{configMapInfo.SecurityGroup},
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName:  aws.String("/dev/sda1"),
			VirtualName: aws.String("ephemeral0"),
			Ebs:         &types.EbsBlockDevice{VolumeSize: aws.Int32(configMapInfo.Disk)},
		}},
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		TagSpecifications:                 []types.TagSpecification{{ResourceType: types.ResourceTypeInstance, Tags: []types.Tag{{Key: aws.String(MultiPlatformManaged), Value: aws.String("true")}, {Key: aws.String(cloud.InstanceTag), Value: aws.String(instanceTag)}, {Key: aws.String("Name"), Value: aws.String("multi-platform-builder-" + name)}}}},
	}

	// Launch the new EC2 instance
	result, err := ec2Client.RunInstances(context.TODO(), launchInput)
	if err != nil {
		return "", err
	}

	// The result will contain information about the newly created instance(s)
	if len(result.Instances) > 0 {
		//hard coded 10m timeout
		return cloud.InstanceIdentifier(*result.Instances[0].InstanceId), nil
	} else {
		return "", fmt.Errorf("no instances were created")
	}
}

func (configMapInfo AwsDynamicConfig) CountInstances(kubeClient client.Client, log *logr.Logger, ctx context.Context, instanceTag string) (int, error) {
	log.Info("attempting to count AWS instances")
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(SecretCredentialsProvider{Name: configMapInfo.Secret, Namespace: configMapInfo.SystemNamespace, Client: kubeClient}),
		config.WithRegion(configMapInfo.Region))
	if err != nil {
		return 0, err
	}

	// Create an EC2 client
	ec2Client := ec2.NewFromConfig(cfg)
	res, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []types.Filter{{Name: aws.String("tag:" + cloud.InstanceTag), Values: []string{instanceTag}}, {Name: aws.String("tag:" + MultiPlatformManaged), Values: []string{"true"}}}})
	if err != nil {
		log.Error(err, "failed to describe instance")
		return 0, err
	}
	count := 0
	for _, res := range res.Reservations {
		for _, inst := range res.Instances {
			if inst.State.Name != types.InstanceStateNameTerminated {
				log.Info(fmt.Sprintf("counting instance %s towards running count", *inst.InstanceId))
				count++
			}
		}
	}
	return count, nil
}

func (configMapInfo AwsDynamicConfig) GetInstanceAddress(kubeClient client.Client, log *logr.Logger, ctx context.Context, instanceId cloud.InstanceIdentifier) (string, error) {
	log.Info(fmt.Sprintf("attempting to get AWS instance address %s", instanceId))
	// Load AWS credentials and configuration

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(SecretCredentialsProvider{Name: configMapInfo.Secret, Namespace: configMapInfo.SystemNamespace, Client: kubeClient}),
		config.WithRegion(configMapInfo.Region))
	if err != nil {
		return "", err
	}

	// Create an EC2 client
	ec2Client := ec2.NewFromConfig(cfg)
	res, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{string(instanceId)}})
	if err != nil {
		log.Error(err, "failed to describe instance")
		return "", err
	}
	if len(res.Reservations) > 0 {
		if len(res.Reservations[0].Instances) > 0 {
			if res.Reservations[0].Instances[0].PublicDnsName != nil && *res.Reservations[0].Instances[0].PublicDnsName != "" {

				server, _ := net.ResolveTCPAddr("tcp", *res.Reservations[0].Instances[0].PublicDnsName+":22")
				conn, err := net.DialTCP("tcp", nil, server)
				if err != nil {
					log.Error(err, "failed to connect to AWS instance")
					return "", err
				}
				defer conn.Close()

				return *res.Reservations[0].Instances[0].PublicDnsName, nil
			}
		}
	}
	return "", nil
}

func (configMapInfo AwsDynamicConfig) TerminateInstance(kubeClient client.Client, log *logr.Logger, ctx context.Context, instance cloud.InstanceIdentifier) error {
	log.Info(fmt.Sprintf("attempting to terminate AWS instance %s", instance))

	// Load AWS credentials and configuration

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(SecretCredentialsProvider{Name: configMapInfo.Secret, Namespace: "multi-platform-controller", Client: kubeClient}),
		config.WithRegion(configMapInfo.Region))
	if err != nil {
		return err
	}

	ec2Client := ec2.NewFromConfig(cfg)
	_, err = ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{string(instance)}})
	return err
}

type SecretCredentialsProvider struct {
	Name      string
	Namespace string
	Client    client.Client
}

func (r SecretCredentialsProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	if r.Client == nil {
		return aws.Credentials{AccessKeyID: os.Getenv("MULTI_ARCH_ACCESS_KEY"), SecretAccessKey: os.Getenv("MULTI_ARCH_SECRET_KEY")}, nil

	}

	s := v1.Secret{}
	err := r.Client.Get(ctx, types2.NamespacedName{Namespace: r.Namespace, Name: r.Name}, &s)
	if err != nil {
		return aws.Credentials{}, err
	}

	return aws.Credentials{AccessKeyID: string(s.Data["access-key-id"]), SecretAccessKey: string(s.Data["secret-access-key"])}, nil
}

type AwsDynamicConfig struct {
	Region          string
	Ami             string
	InstanceType    string
	KeyName         string
	Secret          string
	SystemNamespace string
	SecurityGroup   string
	Disk            int32
}

func (configMapInfo AwsDynamicConfig) SshUser() string {
	return "ec2-user"
}
