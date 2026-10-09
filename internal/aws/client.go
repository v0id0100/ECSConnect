package aws

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwlogstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// Client wraps AWS service clients.
type Client struct {
	ecs    *ecs.Client
	cwlogs *cloudwatchlogs.Client
}

// Cluster holds relevant ECS cluster info.
type Cluster struct {
	Name         string
	ARN          string
	Status       string
	RunningTasks int32
	PendingTasks int32
}

// Container holds info about a task container.
type Container struct {
	Name   string
	Status string
	Image  string
}

// Task holds relevant ECS task info.
type Task struct {
	ARN            string
	ShortID        string
	Status         string
	TaskDefinition string
	TaskDefARN     string
	StartedAt      *time.Time
	Containers     []Container
	CPU            string
	Memory         string
	LaunchType     string
}

// LoadProfiles reads available AWS profiles from ~/.aws/config and ~/.aws/credentials.
func LoadProfiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return []string{"default"}
	}

	seen := make(map[string]bool)
	for _, path := range []string{
		filepath.Join(home, ".aws", "config"),
		filepath.Join(home, ".aws", "credentials"),
	} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				name := line[1 : len(line)-1]
				name = strings.TrimPrefix(name, "profile ")
				if name != "" && name != "DEFAULT" {
					seen[name] = true
				}
			}
		}
		f.Close()
	}

	profiles := make([]string, 0, len(seen))
	for p := range seen {
		profiles = append(profiles, p)
	}
	sort.Strings(profiles)

	if len(profiles) == 0 {
		return []string{"default"}
	}
	return profiles
}

// CommonRegions returns a curated list of AWS regions.
func CommonRegions() []string {
	return []string{
		"us-east-1",
		"us-east-2",
		"us-west-1",
		"us-west-2",
		"eu-west-1",
		"eu-west-2",
		"eu-west-3",
		"eu-central-1",
		"eu-north-1",
		"eu-south-1",
		"ap-southeast-1",
		"ap-southeast-2",
		"ap-northeast-1",
		"ap-northeast-2",
		"ap-south-1",
		"sa-east-1",
		"ca-central-1",
		"af-south-1",
		"me-south-1",
	}
}

// DetectRegion returns the region configured for the given profile, or empty string if not set.
func DetectRegion(ctx context.Context, profile string) string {
	var opts []func(*config.LoadOptions) error
	if profile != "" && profile != "default" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return ""
	}
	return cfg.Region
}

// NewClient creates a new AWS client configured with the given profile and region.
func NewClient(ctx context.Context, profile, region string) (*Client, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" && profile != "default" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &Client{
		ecs:    ecs.NewFromConfig(cfg),
		cwlogs: cloudwatchlogs.NewFromConfig(cfg),
	}, nil
}

// ListClusters returns all ECS clusters in the account/region.
func (c *Client) ListClusters(ctx context.Context) ([]Cluster, error) {
	var arns []string
	var nextToken *string
	for {
		out, err := c.ecs.ListClusters(ctx, &ecs.ListClustersInput{NextToken: nextToken})
		if err != nil {
			return nil, fmt.Errorf("listing clusters: %w", err)
		}
		arns = append(arns, out.ClusterArns...)
		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	if len(arns) == 0 {
		return nil, nil
	}

	out, err := c.ecs.DescribeClusters(ctx, &ecs.DescribeClustersInput{Clusters: arns})
	if err != nil {
		return nil, fmt.Errorf("describing clusters: %w", err)
	}

	clusters := make([]Cluster, 0, len(out.Clusters))
	for _, cl := range out.Clusters {
		clusters = append(clusters, Cluster{
			Name:         aws.ToString(cl.ClusterName),
			ARN:          aws.ToString(cl.ClusterArn),
			Status:       aws.ToString(cl.Status),
			RunningTasks: cl.RunningTasksCount,
			PendingTasks: cl.PendingTasksCount,
		})
	}
	return clusters, nil
}

// ListTasks returns all tasks in the given cluster.
func (c *Client) ListTasks(ctx context.Context, clusterName string) ([]Task, error) {
	var taskARNs []string
	var nextToken *string
	for {
		out, err := c.ecs.ListTasks(ctx, &ecs.ListTasksInput{
			Cluster:   aws.String(clusterName),
			NextToken: nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("listing tasks: %w", err)
		}
		taskARNs = append(taskARNs, out.TaskArns...)
		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	if len(taskARNs) == 0 {
		return nil, nil
	}

	var tasks []Task
	for i := 0; i < len(taskARNs); i += 100 {
		end := i + 100
		if end > len(taskARNs) {
			end = len(taskARNs)
		}
		out, err := c.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: aws.String(clusterName),
			Tasks:   taskARNs[i:end],
		})
		if err != nil {
			return nil, fmt.Errorf("describing tasks: %w", err)
		}
		for _, t := range out.Tasks {
			tasks = append(tasks, parseTask(t))
		}
	}
	return tasks, nil
}

func parseTask(t ecstypes.Task) Task {
	shortID := aws.ToString(t.TaskArn)
	if idx := strings.LastIndex(shortID, "/"); idx >= 0 {
		shortID = shortID[idx+1:]
	}

	taskDefARN := aws.ToString(t.TaskDefinitionArn)
	taskDef := taskDefARN
	if idx := strings.LastIndex(taskDef, "/"); idx >= 0 {
		taskDef = taskDef[idx+1:]
	}

	containers := make([]Container, 0, len(t.Containers))
	for _, cont := range t.Containers {
		img := aws.ToString(cont.Image)
		if idx := strings.LastIndex(img, "/"); idx >= 0 {
			img = img[idx+1:]
		}
		containers = append(containers, Container{
			Name:   aws.ToString(cont.Name),
			Status: aws.ToString(cont.LastStatus),
			Image:  img,
		})
	}

	launchType := string(t.LaunchType)
	if launchType == "" {
		launchType = "FARGATE"
	}

	return Task{
		ARN:            aws.ToString(t.TaskArn),
		ShortID:        shortID,
		Status:         aws.ToString(t.LastStatus),
		TaskDefinition: taskDef,
		TaskDefARN:     taskDefARN,
		StartedAt:      t.StartedAt,
		Containers:     containers,
		CPU:            aws.ToString(t.Cpu),
		Memory:         aws.ToString(t.Memory),
		LaunchType:     launchType,
	}
}

// LogStream holds the CloudWatch log group and stream name for a container.
type LogStream struct {
	Group  string
	Stream string
}

// GetContainerLogs fetches the most recent log events and returns the stream
// info + NextForwardToken needed to poll for live updates.
func (c *Client) GetContainerLogs(ctx context.Context, taskDefARN, taskARN, containerName string, limit int32) ([]string, LogStream, string, error) {
	tdOut, err := c.ecs.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefARN),
	})
	if err != nil {
		return nil, LogStream{}, "", fmt.Errorf("describing task definition: %w", err)
	}

	var logGroup, logPrefix string
	for _, cd := range tdOut.TaskDefinition.ContainerDefinitions {
		if aws.ToString(cd.Name) != containerName {
			continue
		}
		if cd.LogConfiguration == nil || cd.LogConfiguration.Options == nil {
			return nil, LogStream{}, "", fmt.Errorf("container %q does not have CloudWatch logs configured", containerName)
		}
		logGroup = cd.LogConfiguration.Options["awslogs-group"]
		logPrefix = cd.LogConfiguration.Options["awslogs-stream-prefix"]
		break
	}

	if logGroup == "" {
		return nil, LogStream{}, "", fmt.Errorf("container %q does not have CloudWatch logs configured", containerName)
	}

	taskID := taskARN
	if idx := strings.LastIndex(taskID, "/"); idx >= 0 {
		taskID = taskID[idx+1:]
	}

	stream := LogStream{
		Group:  logGroup,
		Stream: fmt.Sprintf("%s/%s/%s", logPrefix, containerName, taskID),
	}

	out, err := c.cwlogs.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(stream.Group),
		LogStreamName: aws.String(stream.Stream),
		Limit:         aws.Int32(limit),
		StartFromHead: aws.Bool(false),
	})
	if err != nil {
		return nil, LogStream{}, "", fmt.Errorf("fetching logs (stream: %s): %w", stream.Stream, err)
	}

	lines := eventsToLines(out.Events)
	return lines, stream, aws.ToString(out.NextForwardToken), nil
}

// PollLogs fetches log events that arrived after the given forward token.
// Returns new lines (empty if none) and the updated token.
func (c *Client) PollLogs(ctx context.Context, stream LogStream, forwardToken string, limit int32) ([]string, string, error) {
	out, err := c.cwlogs.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(stream.Group),
		LogStreamName: aws.String(stream.Stream),
		NextToken:     aws.String(forwardToken),
		Limit:         aws.Int32(limit),
		StartFromHead: aws.Bool(true),
	})
	if err != nil {
		return nil, forwardToken, fmt.Errorf("polling logs: %w", err)
	}
	return eventsToLines(out.Events), aws.ToString(out.NextForwardToken), nil
}

func eventsToLines(events []cwlogstypes.OutputLogEvent) []string {
	lines := make([]string, 0, len(events))
	for _, e := range events {
		ts := time.UnixMilli(aws.ToInt64(e.Timestamp))
		lines = append(lines, fmt.Sprintf("[%s] %s", ts.Format("15:04:05"), aws.ToString(e.Message)))
	}
	return lines
}
