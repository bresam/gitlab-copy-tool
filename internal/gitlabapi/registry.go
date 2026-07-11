package gitlabapi

import (
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// ContainerImage is one container-registry repository (image) and its tags.
type ContainerImage struct {
	Location string // full image path incl. registry host, e.g. registry.gitlab.com/grp/proj/img
	Tags     []string
}

// ContainerImages lists a project's container-registry images with their tags.
func (c *Client) ContainerImages(pid int64) ([]ContainerImage, error) {
	opt := &gitlab.ListProjectRegistryRepositoriesOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
		Tags:        gitlab.Ptr(true),
	}
	var out []ContainerImage
	for {
		repos, resp, err := c.GL.ContainerRegistry.ListProjectRegistryRepositories(pid, opt)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			img := ContainerImage{Location: r.Location}
			for _, t := range r.Tags {
				img.Tags = append(img.Tags, t.Name)
			}
			if len(img.Tags) == 0 {
				img.Tags, _ = c.registryTags(pid, r.ID)
			}
			out = append(out, img)
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (c *Client) registryTags(pid, repoID int64) ([]string, error) {
	opt := &gitlab.ListRegistryRepositoryTagsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var names []string
	for {
		tags, resp, err := c.GL.ContainerRegistry.ListRegistryRepositoryTags(pid, repoID, opt)
		if err != nil {
			return names, err
		}
		for _, t := range tags {
			names = append(names, t.Name)
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return names, nil
}

// ProjectImagePrefix returns the container-registry image prefix of a project
// (e.g. registry.gitlab.com/group/project), or "" if the registry is disabled.
func (c *Client) ProjectImagePrefix(pid int64) (string, error) {
	p, _, err := c.GL.Projects.GetProject(pid, nil)
	if err != nil {
		return "", err
	}
	return p.ContainerRegistryImagePrefix, nil
}

// CurrentUsername returns the authenticated user's login.
func (c *Client) CurrentUsername() (string, error) {
	u, _, err := c.GL.Users.CurrentUser()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}
