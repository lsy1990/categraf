//go:build !no_logs

// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"

	"flashcat.cloud/categraf/logs/util/containers/providers"
	"flashcat.cloud/categraf/pkg/cache"
	"flashcat.cloud/categraf/pkg/retry"
)

// DockerUtil wraps interactions with a local docker API.
type DockerUtil struct {
	// used to setup the DockerUtil
	initRetry retry.Retrier

	sync.Mutex
	cfg          *Config
	cli          *client.Client
	queryTimeout time.Duration
	// tracks the last time we invalidate our internal caches
	lastInvalidate time.Time
	// networkMappings by container id
	networkMappings map[string][]dockerNetwork
	// image sha mapping cache
	imageNameBySha map[string]string
	// event subscribers and state
	eventState *eventStreamState
}

// init makes an empty DockerUtil bootstrap itself.
// This is not exposed as public API but is called by the retrier embed.
func (d *DockerUtil) init() error {
	// TODO
	// d.queryTimeout = config.GetDuration("docker_query_timeout") * time.Second
	d.queryTimeout = 5 * time.Second

	// Major failure risk is here, do that first
	ctx, cancel := context.WithTimeout(context.Background(), d.queryTimeout)
	defer cancel()
	cli, err := ConnectToDocker(ctx)
	if err != nil {
		return err
	}

	cfg := &Config{
		// TODO: bind them to config entries if relevant
		CollectNetwork: true,
		CacheDuration:  10 * time.Second,
	}

	d.cfg = cfg
	d.cli = cli
	d.networkMappings = make(map[string][]dockerNetwork)
	d.imageNameBySha = make(map[string]string)
	d.lastInvalidate = time.Now()
	d.eventState = newEventStreamState()

	return nil
}

// ConnectToDocker connects to docker and negotiates the API version
func ConnectToDocker(ctx context.Context) (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	// Looks like docker is not actually doing a call to server when `NewClient` is called
	// Forcing it to verify server availability by calling Info()
	_, err = cli.Info(ctx)
	if err != nil {
		return nil, err
	}

	log.Println("Successfully connected to Docker server")

	return cli, nil
}

// Images returns a slice of all images.
func (d *DockerUtil) Images(ctx context.Context, includeIntermediate bool) ([]types.ImageSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
	defer cancel()
	images, err := d.cli.ImageList(ctx, types.ImageListOptions{All: includeIntermediate})

	if err != nil {
		return nil, fmt.Errorf("unable to list docker images: %s", err)
	}
	return images, nil
}

// CountVolumes returns the number of attached and dangling volumes.
func (d *DockerUtil) CountVolumes(ctx context.Context) (int, int, error) {
	attachedFilter, _ := buildDockerFilter("dangling", "false")
	danglingFilter, _ := buildDockerFilter("dangling", "true")
	ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
	defer cancel()

	attachedVolumes, err := d.cli.VolumeList(ctx, attachedFilter)
	if err != nil {
		return 0, 0, fmt.Errorf("unable to list attached docker volumes: %s", err)
	}
	danglingVolumes, err := d.cli.VolumeList(ctx, danglingFilter)
	if err != nil {
		return 0, 0, fmt.Errorf("unable to list dangling docker volumes: %s", err)
	}

	return len(attachedVolumes.Volumes), len(danglingVolumes.Volumes), nil
}

// RawContainerList wraps around the docker client's ContainerList method.
// Value validation and error handling are the caller's responsibility.
func (d *DockerUtil) RawContainerList(ctx context.Context, options types.ContainerListOptions) ([]types.Container, error) {
	ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
	defer cancel()
	return d.cli.ContainerList(ctx, options)
}

func (d *DockerUtil) GetHostname(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
	defer cancel()
	info, err := d.cli.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("unable to get Docker info: %s", err)
	}
	return info.Name, nil
}

// GetStorageStats returns the docker global storage stats if available
// or ErrStorageStatsNotAvailable
func (d *DockerUtil) GetStorageStats(ctx context.Context) ([]*StorageStats, error) {
	ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
	defer cancel()
	info, err := d.cli.Info(ctx)
	if err != nil {
		return []*StorageStats{}, fmt.Errorf("unable to get Docker info: %s", err)
	}
	return parseStorageStatsFromInfo(info)
}

func isImageShaOrRepoDigest(image string) bool {
	return strings.HasPrefix(image, "sha256:") || strings.Contains(image, "@sha256:")
}

// ResolveImageName will resolve sha image name to their user-friendly name.
// For non-sha/non-repodigest names we will just return the name as-is.
func (d *DockerUtil) ResolveImageName(ctx context.Context, image string) (string, error) {
	if !isImageShaOrRepoDigest(image) {
		return image, nil
	}

	d.Lock()
	defer d.Unlock()
	if _, ok := d.imageNameBySha[image]; !ok {
		ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
		defer cancel()
		r, _, err := d.cli.ImageInspectWithRaw(ctx, image)
		if err != nil {
			// Only log errors that aren't "not found" because some images may
			// just not be available in docker inspect.
			if !client.IsErrNotFound(err) {
				return image, err
			}
			d.imageNameBySha[image] = image
		}

		// Try RepoTags first and fall back to RepoDigest otherwise.
		if len(r.RepoTags) > 0 {
			sort.Strings(r.RepoTags)
			d.imageNameBySha[image] = r.RepoTags[0]
		} else if len(r.RepoDigests) > 0 {
			// Digests formatted like quay.io/foo/bar@sha256:hash
			sort.Strings(r.RepoDigests)
			sp := strings.SplitN(r.RepoDigests[0], "@", 2)
			d.imageNameBySha[image] = sp[0]
		} else {
			log.Printf("No information in image/inspect to resolve: %s", image)
			d.imageNameBySha[image] = image
		}
	}
	return d.imageNameBySha[image], nil
}

// ResolveImageNameFromContainer will resolve the container sha image name to their user-friendly name.
// It is similar to ResolveImageName except it tries to match the image to the container Config.Image.
// For non-sha names we will just return the name as-is.
func (d *DockerUtil) ResolveImageNameFromContainer(ctx context.Context, co types.ContainerJSON) (string, error) {
	if co.Config.Image != "" && !isImageShaOrRepoDigest(co.Config.Image) {
		return co.Config.Image, nil
	}

	return d.ResolveImageName(ctx, co.Image)
}

// Inspect returns a docker inspect object for a given container ID.
// It tries to locate the container in the inspect cache before making the docker inspect call
func (d *DockerUtil) Inspect(ctx context.Context, id string, withSize bool) (types.ContainerJSON, error) {
	cacheKey := GetInspectCacheKey(id, withSize)
	var container types.ContainerJSON

	cached, hit := cache.Cache.Get(cacheKey)
	// Try to get sized hit if we got a miss and withSize=false
	if !hit && !withSize {
		cached, hit = cache.Cache.Get(GetInspectCacheKey(id, true))
	}

	if hit {
		container, ok := cached.(types.ContainerJSON)
		if !ok {
			log.Println("Invalid inspect cache format, forcing a cache miss")
		} else {
			return container, nil
		}
	}

	container, err := d.InspectNoCache(ctx, id, withSize)
	if err != nil {
		return container, err
	}

	// cache the inspect for 10 seconds to reduce pressure on the daemon
	cache.Cache.Set(cacheKey, container, 10*time.Second)

	return container, nil
}

// InspectNoCache returns a docker inspect object for a given container ID. It
// ignores the inspect cache, always collecting fresh data from the docker
// daemon.
func (d *DockerUtil) InspectNoCache(ctx context.Context, id string, withSize bool) (types.ContainerJSON, error) {
	ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
	defer cancel()

	container, _, err := d.cli.ContainerInspectWithRaw(ctx, id, withSize)
	if client.IsErrNotFound(err) {
		return container, fmt.Errorf("docker container %s", id)
	}
	if err != nil {
		return container, err
	}

	// ContainerJSONBase is a pointer embed, so it might be nil and cause segfaults
	if container.ContainerJSONBase == nil {
		return container, errors.New("invalid inspect data")
	}

	return container, nil
}

// InspectSelf returns the inspect content of the container the current agent is running in
func (d *DockerUtil) InspectSelf(ctx context.Context) (types.ContainerJSON, error) {
	cID, err := providers.ContainerImpl().GetAgentCID()
	if err != nil {
		return types.ContainerJSON{}, err
	}

	return d.Inspect(ctx, cID, false)
}

// AllContainerLabels retrieves all running containers (`docker ps`) and returns
// a map mapping containerID to container labels as a map[string]string
func (d *DockerUtil) AllContainerLabels(ctx context.Context) (map[string]map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
	defer cancel()
	containers, err := d.cli.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing containers: %s", err)
	}

	labelMap := make(map[string]map[string]string)

	for _, container := range containers {
		if len(container.ID) == 0 {
			continue
		}
		labelMap[container.ID] = container.Labels
	}

	return labelMap, nil
}

func (d *DockerUtil) GetContainerStats(ctx context.Context, containerID string) (*types.StatsJSON, error) {
	ctx, cancel := context.WithTimeout(ctx, d.queryTimeout)
	defer cancel()
	stats, err := d.cli.ContainerStats(ctx, containerID, false)
	if err != nil {
		return nil, fmt.Errorf("unable to get Docker stats: %s", err)
	}
	containerStats := &types.StatsJSON{}
	err = json.NewDecoder(stats.Body).Decode(&containerStats)
	if err != nil {
		return nil, fmt.Errorf("error listing containers: %s", err)
	}
	return containerStats, nil
}
