/*
Copyright 2019 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cruntime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"
	"k8s.io/minikube/pkg/minikube/assets"
	"k8s.io/minikube/pkg/minikube/bootstrapper/images"
	"k8s.io/minikube/pkg/minikube/cni"
	"k8s.io/minikube/pkg/minikube/command"
	"k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/download"
	"k8s.io/minikube/pkg/minikube/style"
	"k8s.io/minikube/pkg/minikube/sysinit"
)

const (
	containerdNamespaceRoot = "/run/containerd/runc/k8s.io"
	// ContainerdConfFile is the path to the containerd configuration
	containerdConfigFile  = "/etc/containerd/config.toml"
	containerdMirrorsRoot = "/etc/containerd/certs.d"
	//containerdImportedConfigFile = "/etc/containerd/containerd.conf.d/02-containerd.conf"
	containerdInsecureRegistryTemplate = `server = "{{.InsecureRegistry -}}"

[host."{{.InsecureRegistry -}}"]
  skip_verify = true
`
)

// Containerd contains containerd runtime state
type Containerd struct {
	Socket            string
	Runner            CommandRunner
	ImageRepository   string
	KubernetesVersion semver.Version
	Init              sysinit.Manager
	InsecureRegistry  []string
}

// Name is a human readable name for containerd
func (r *Containerd) Name() string {
	return "containerd"
}

// Style is the console style for containerd
func (r *Containerd) Style() style.Enum {
	return style.Containerd
}

// parseContainerdVersion parses version from containerd --version
func parseContainerdVersion(line string) (string, error) {
	// containerd github.com/containerd/containerd v1.0.0 89623f28b87a6004d4b785663257362d1658a729
	words := strings.Split(line, " ")
	if len(words) >= 4 && words[0] == "containerd" {
		version := strings.Replace(words[2], "v", "", 1)
		version = strings.SplitN(version, "~", 2)[0]
		if _, err := semver.Parse(version); err != nil {
			parts := strings.SplitN(version, "-", 2)
			return parts[0], nil
		}
		return version, nil
	}
	return "", fmt.Errorf("unknown version: %q", line)
}

// Version retrieves the current version of this runtime
func (r *Containerd) Version() (string, error) {
	c := exec.Command("containerd", "--version")
	rr, err := r.Runner.RunCmd(c)
	if err != nil {
		return "", errors.Wrapf(err, "containerd check version")
	}
	version, err := parseContainerdVersion(rr.Stdout.String())
	if err != nil {
		return "", err
	}
	return version, nil
}

// SocketPath returns the path to the socket file for containerd
func (r *Containerd) SocketPath() string {
	if r.Socket != "" {
		return r.Socket
	}
	return "/run/containerd/containerd.sock"
}

// Active returns if containerd is active on the host
func (r *Containerd) Active() bool {
	return r.Init.Active("containerd")
}

// Available returns an error if it is not possible to use this runtime on a host
func (r *Containerd) Available() error {
	c := exec.Command("which", "containerd")
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "check containerd availability")
	}
	return nil
}

// generateContainerdConfig sets up /etc/containerd/config.toml & /etc/containerd/containerd.conf.d/02-containerd.conf
func generateContainerdConfig(cr CommandRunner, imageRepository string, kv semver.Version, forceSystemd bool, insecureRegistry []string, inUserNamespace bool) error {
	pauseImage := images.Pause(kv, imageRepository)
	if _, err := cr.RunCmd(exec.Command("/bin/bash", "-c", fmt.Sprintf("sudo sed -e 's|^.*sandbox_image = .*$|sandbox_image = \"%s\"|' -i %s", pauseImage, containerdConfigFile))); err != nil {
		return errors.Wrap(err, "update sandbox_image")
	}
	if _, err := cr.RunCmd(exec.Command("/bin/bash", "-c", fmt.Sprintf("sudo sed -e 's|^.*restrict_oom_score_adj = .*$|restrict_oom_score_adj = %t|' -i %s", inUserNamespace, containerdConfigFile))); err != nil {
		return errors.Wrap(err, "update restrict_oom_score_adj")
	}
	if _, err := cr.RunCmd(exec.Command("/bin/bash", "-c", fmt.Sprintf("sudo sed -e 's|^.*SystemdCgroup = .*$|SystemdCgroup = %t|' -i %s", forceSystemd, containerdConfigFile))); err != nil {
		return errors.Wrap(err, "update SystemdCgroup")
	}
	if _, err := cr.RunCmd(exec.Command("/bin/bash", "-c", fmt.Sprintf("sudo sed -e 's|^.*conf_dir = .*$|conf_dir = \"%s\"|' -i %s", cni.ConfDir, containerdConfigFile))); err != nil {
		return errors.Wrap(err, "update conf_dir")
	}

	for _, registry := range insecureRegistry {
		addr := registry
		if strings.HasPrefix(strings.ToLower(registry), "http://") || strings.HasPrefix(strings.ToLower(registry), "https://") {
			i := strings.Index(addr, "//")
			addr = addr[i+2:]
		} else {
			registry = "http://" + registry
		}

		t, err := template.New("hosts.toml").Parse(containerdInsecureRegistryTemplate)
		if err != nil {
			return errors.Wrap(err, "unable to parse insecure registry template")
		}
		opts := struct {
			InsecureRegistry string
		}{
			InsecureRegistry: registry,
		}
		var b bytes.Buffer
		if err := t.Execute(&b, opts); err != nil {
			return errors.Wrap(err, "unable to create insecure registry template")
		}
		regRootPath := path.Join(containerdMirrorsRoot, addr)

		c := exec.Command("/bin/bash", "-c", fmt.Sprintf("sudo mkdir -p %s && printf %%s \"%s\" | base64 -d | sudo tee %s", regRootPath, base64.StdEncoding.EncodeToString(b.Bytes()), path.Join(regRootPath, "hosts.toml")))
		if _, err := cr.RunCmd(c); err != nil {
			return errors.Wrap(err, "unable to generate insecure registry cfg")
		}
	}
	return nil
}

// Enable idempotently enables containerd on a host
func (r *Containerd) Enable(disOthers, forceSystemd, inUserNamespace bool) error {
	if inUserNamespace {
		if err := CheckKernelCompatibility(r.Runner, 5, 11); err != nil {
			// For using overlayfs
			return fmt.Errorf("kernel >= 5.11 is required for rootless mode: %w", err)
		}
		if err := CheckKernelCompatibility(r.Runner, 5, 13); err != nil {
			// For avoiding SELinux error with overlayfs
			klog.Warningf("kernel >= 5.13 is recommended for rootless mode %v", err)
		}
	}
	if disOthers {
		if err := disableOthers(r, r.Runner); err != nil {
			klog.Warningf("disableOthers: %v", err)
		}
	}
	if err := populateCRIConfig(r.Runner, r.SocketPath()); err != nil {
		return err
	}
	if err := generateContainerdConfig(r.Runner, r.ImageRepository, r.KubernetesVersion, forceSystemd, r.InsecureRegistry, inUserNamespace); err != nil {
		return err
	}
	if err := enableIPForwarding(r.Runner); err != nil {
		return err
	}

	// Otherwise, containerd will fail API requests with 'Unimplemented'
	return r.Init.Restart("containerd")
}

// Disable idempotently disables containerd on a host
func (r *Containerd) Disable() error {
	return r.Init.ForceStop("containerd")
}

// ImageExists checks if image exists based on image name and optionally image sha
func (r *Containerd) ImageExists(name string, sha string) bool {
	c := exec.Command("/bin/bash", "-c", fmt.Sprintf("sudo ctr -n=k8s.io images check | grep %s", name))
	rr, err := r.Runner.RunCmd(c)
	if err != nil {
		return false
	}
	if sha != "" && !strings.Contains(rr.Output(), sha) {
		return false
	}
	return true
}

// ListImages lists images managed by this container runtime
func (r *Containerd) ListImages(ListImagesOptions) ([]ListImage, error) {
	return listCRIImages(r.Runner)
}

// LoadImage loads an image into this runtime
func (r *Containerd) LoadImage(path string) error {
	klog.Infof("Loading image: %s", path)
	c := exec.Command("sudo", "ctr", "-n=k8s.io", "images", "import", path)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrapf(err, "ctr images import")
	}
	return nil
}

// PullImage pulls an image into this runtime
func (r *Containerd) PullImage(name string) error {
	return pullCRIImage(r.Runner, name)
}

// SaveImage save an image from this runtime
func (r *Containerd) SaveImage(name string, path string) error {
	klog.Infof("Saving image %s: %s", name, path)
	c := exec.Command("sudo", "ctr", "-n=k8s.io", "images", "export", path, name)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrapf(err, "ctr images export")
	}
	return nil
}

// RemoveImage removes a image
func (r *Containerd) RemoveImage(name string) error {
	return removeCRIImage(r.Runner, name)
}

// TagImage tags an image in this runtime
func (r *Containerd) TagImage(source string, target string) error {
	klog.Infof("Tagging image %s: %s", source, target)
	c := exec.Command("sudo", "ctr", "-n=k8s.io", "images", "tag", source, target)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrapf(err, "ctr images tag")
	}
	return nil
}

func gitClone(cr CommandRunner, src string) (string, error) {
	// clone to a temporary directory
	rr, err := cr.RunCmd(exec.Command("mktemp", "-d"))
	if err != nil {
		return "", err
	}
	tmp := strings.TrimSpace(rr.Stdout.String())
	cmd := exec.Command("git", "clone", src, tmp)
	if _, err := cr.RunCmd(cmd); err != nil {
		return "", err
	}
	return tmp, nil
}

func downloadRemote(cr CommandRunner, src string) (string, error) {
	u, err := url.Parse(src)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" && u.Host == "" { // regular file, return
		return src, nil
	}
	if u.Scheme == "git" {
		return gitClone(cr, src)
	}

	// download to a temporary file
	rr, err := cr.RunCmd(exec.Command("mktemp"))
	if err != nil {
		return "", err
	}
	dst := strings.TrimSpace(rr.Stdout.String())
	cmd := exec.Command("curl", "-L", "-o", dst, src)
	if _, err := cr.RunCmd(cmd); err != nil {
		return "", err
	}

	// extract to a temporary directory
	rr, err = cr.RunCmd(exec.Command("mktemp", "-d"))
	if err != nil {
		return "", err
	}
	tmp := strings.TrimSpace(rr.Stdout.String())
	cmd = exec.Command("tar", "-C", tmp, "-xf", dst)
	if _, err := cr.RunCmd(cmd); err != nil {
		return "", err
	}

	return tmp, nil
}

// BuildImage builds an image into this runtime
func (r *Containerd) BuildImage(src string, file string, tag string, push bool, env []string, opts []string) error {
	// download url if not already present
	dir, err := downloadRemote(r.Runner, src)
	if err != nil {
		return err
	}
	if file != "" {
		if dir != src {
			file = path.Join(dir, file)
		}
		// copy to standard path for Dockerfile
		df := path.Join(dir, "Dockerfile")
		if file != df {
			cmd := exec.Command("sudo", "cp", "-f", file, df)
			if _, err := r.Runner.RunCmd(cmd); err != nil {
				return err
			}
		}
	}
	klog.Infof("Building image: %s", dir)
	extra := ""
	if tag != "" {
		// add default tag if missing
		if !strings.Contains(tag, ":") {
			tag += ":latest"
		}
		extra = fmt.Sprintf(",name=%s", tag)
		if push {
			extra += ",push=true"
		}
	}
	args := []string{"buildctl", "build",
		"--frontend", "dockerfile.v0",
		"--local", fmt.Sprintf("context=%s", dir),
		"--local", fmt.Sprintf("dockerfile=%s", dir),
		"--output", fmt.Sprintf("type=image%s", extra)}
	for _, opt := range opts {
		args = append(args, "--"+opt)
	}
	c := exec.Command("sudo", args...)
	e := os.Environ()
	e = append(e, env...)
	c.Env = e
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "buildctl build")
	}
	return nil
}

// PushImage pushes an image
func (r *Containerd) PushImage(name string) error {
	klog.Infof("Pushing image %s: %s", name)
	c := exec.Command("sudo", "ctr", "-n=k8s.io", "images", "push", name)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrapf(err, "ctr images push")
	}
	return nil
}

// CGroupDriver returns cgroup driver ("cgroupfs" or "systemd")
func (r *Containerd) CGroupDriver() (string, error) {
	info, err := getCRIInfo(r.Runner)
	if err != nil {
		return "", err
	}
	if info["config"] == nil {
		return "", errors.Wrapf(err, "missing config")
	}
	config, ok := info["config"].(map[string]interface{})
	if !ok {
		return "", errors.Wrapf(err, "config not map")
	}
	cgroupManager := "cgroupfs" // default
	switch config["systemdCgroup"] {
	case false:
		cgroupManager = "cgroupfs"
	case true:
		cgroupManager = "systemd"
	}
	return cgroupManager, nil
}

// KubeletOptions returns kubelet options for a containerd
func (r *Containerd) KubeletOptions() map[string]string {
	return map[string]string{
		"container-runtime":          "remote",
		"container-runtime-endpoint": fmt.Sprintf("unix://%s", r.SocketPath()),
		"image-service-endpoint":     fmt.Sprintf("unix://%s", r.SocketPath()),
		"runtime-request-timeout":    "15m",
	}
}

// ListContainers returns a list of managed by this container runtime
func (r *Containerd) ListContainers(o ListContainersOptions) ([]string, error) {
	return listCRIContainers(r.Runner, containerdNamespaceRoot, o)
}

// PauseContainers pauses a running container based on ID
func (r *Containerd) PauseContainers(ids []string) error {
	return pauseCRIContainers(r.Runner, containerdNamespaceRoot, ids)
}

// UnpauseContainers unpauses a running container based on ID
func (r *Containerd) UnpauseContainers(ids []string) error {
	return unpauseCRIContainers(r.Runner, containerdNamespaceRoot, ids)
}

// KillContainers removes containers based on ID
func (r *Containerd) KillContainers(ids []string) error {
	return killCRIContainers(r.Runner, ids)
}

// StopContainers stops containers based on ID
func (r *Containerd) StopContainers(ids []string) error {
	return stopCRIContainers(r.Runner, ids)
}

// ContainerLogCmd returns the command to retrieve the log for a container based on ID
func (r *Containerd) ContainerLogCmd(id string, len int, follow bool) string {
	return criContainerLogCmd(r.Runner, id, len, follow)
}

// SystemLogCmd returns the command to retrieve system logs
func (r *Containerd) SystemLogCmd(len int) string {
	return fmt.Sprintf("sudo journalctl -u containerd -n %d", len)
}

// Preload preloads the container runtime with k8s images
func (r *Containerd) Preload(cc config.ClusterConfig) error {
	if !download.PreloadExists(cc.KubernetesConfig.KubernetesVersion, cc.KubernetesConfig.ContainerRuntime, cc.Driver) {
		return nil
	}

	k8sVersion := cc.KubernetesConfig.KubernetesVersion
	cRuntime := cc.KubernetesConfig.ContainerRuntime

	// If images already exist, return
	images, err := images.Kubeadm(cc.KubernetesConfig.ImageRepository, k8sVersion)
	if err != nil {
		return errors.Wrap(err, "getting images")
	}
	if containerdImagesPreloaded(r.Runner, images) {
		klog.Info("Images already preloaded, skipping extraction")
		return nil
	}

	tarballPath := download.TarballPath(k8sVersion, cRuntime)
	targetDir := "/"
	targetName := "preloaded.tar.lz4"
	dest := path.Join(targetDir, targetName)

	c := exec.Command("which", "lz4")
	if _, err := r.Runner.RunCmd(c); err != nil {
		return NewErrISOFeature("lz4")
	}

	// Copy over tarball into host
	fa, err := assets.NewFileAsset(tarballPath, targetDir, targetName, "0644")
	if err != nil {
		return errors.Wrap(err, "getting file asset")
	}
	defer func() {
		if err := fa.Close(); err != nil {
			klog.Warningf("error closing the file %s: %v", fa.GetSourcePath(), err)
		}
	}()

	t := time.Now()
	if err := r.Runner.Copy(fa); err != nil {
		return errors.Wrap(err, "copying file")
	}
	klog.Infof("Took %f seconds to copy over tarball", time.Since(t).Seconds())

	t = time.Now()
	// extract the tarball to /var in the VM
	if rr, err := r.Runner.RunCmd(exec.Command("sudo", "tar", "-I", "lz4", "-C", "/var", "-xf", dest)); err != nil {
		return errors.Wrapf(err, "extracting tarball: %s", rr.Output())
	}
	klog.Infof("Took %f seconds t extract the tarball", time.Since(t).Seconds())

	//  remove the tarball in the VM
	if err := r.Runner.Remove(fa); err != nil {
		klog.Infof("error removing tarball: %v", err)
	}

	return r.Restart()
}

// Restart restarts Docker on a host
func (r *Containerd) Restart() error {
	return r.Init.Restart("containerd")
}

// containerdImagesPreloaded returns true if all images have been preloaded
func containerdImagesPreloaded(runner command.Runner, images []string) bool {
	rr, err := runner.RunCmd(exec.Command("sudo", "crictl", "images", "--output", "json"))
	if err != nil {
		return false
	}

	var jsonImages crictlImages
	err = json.Unmarshal(rr.Stdout.Bytes(), &jsonImages)
	if err != nil {
		klog.Errorf("failed to unmarshal images, will assume images are not preloaded")
		return false
	}

	// Make sure images == imgs
	for _, i := range images {
		found := false
		for _, ji := range jsonImages.Images {
			for _, rt := range ji.RepoTags {
				i = addRepoTagToImageName(i)
				if i == rt {
					found = true
					break
				}
			}
			if found {
				break
			}

		}
		if !found {
			klog.Infof("couldn't find preloaded image for %q. assuming images are not preloaded.", i)
			return false
		}
	}
	klog.Infof("all images are preloaded for containerd runtime.")
	return true
}

// ImagesPreloaded returns true if all images have been preloaded
func (r *Containerd) ImagesPreloaded(images []string) bool {
	return containerdImagesPreloaded(r.Runner, images)
}
