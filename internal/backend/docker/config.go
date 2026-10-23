package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"boxpool.run/boxpool/internal/backend"
)

// DefaultImage is the base guest image release v0.0.1-alpha.1, pinned by
// the digest of its two-architecture index, the same release as the
// Firecracker artifacts.
const DefaultImage = "ghcr.io/sukhendu2002/boxpool-image@sha256:747ea3a41c0dd069a535af68807ec03794b4876676bbc5aabbb71b0801301eba"

// Warning is what every entry point that picks or creates Docker sandboxes
// shows: the fallback is weaker than the microVM backends.
const Warning = "fallback mode: Docker sandboxes are containers that share the host's kernel, so isolation is weaker than a Firecracker microVM"

// Labels on every container, network and volume the driver creates. A
// resource is the driver's only if owner is its sandboxes directory and
// sandbox is the ID its name was made from.
const (
	labelOwner   = "run.boxpool.owner"
	labelSandbox = "run.boxpool.sandbox"
	labelName    = "run.boxpool.name" // Spec.Name, on containers
)

// Paths inside the container.
const (
	agentDir  = "/run/boxpool" // bind mount of the host's agent directory
	agentSock = agentDir + "/agent.sock"
	homeDir   = "/root" // the volume: HOME, the working directory and the only persistent path
)

// pidsLimit caps processes per sandbox, against fork bombs.
const pidsLimit = 4096

// agentCmd starts the image's agent on the bind-mounted socket. The agent
// runs as root with no capabilities, so its socket (mode 0755 under the
// default umask) would refuse the unprivileged host daemon; making it 0666
// is safe because the host directory above it is 0700. A umask would leak
// into every command the agent runs, and the agent in published images
// cannot be changed. The stale socket goes first, or the wait could see it.
const agentCmd = `rm -f ` + agentSock + `
/usr/local/bin/boxpool-agent --listen unix:` + agentSock + ` &
agent=$!
while ! [ -S ` + agentSock + ` ] && kill -0 $agent 2>/dev/null; do sleep 0.05; done
chmod 0666 ` + agentSock + `
wait $agent`

// validID allows the characters Docker allows in names, so the ID can name
// resources and directories as it is.
var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`).MatchString

func checkID(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid sandbox ID %q: want 1-64 letters, digits, '_', '.' or '-', starting with a letter or digit", id)
	}
	return nil
}

// prefix is the start of every resource name for sandboxes in root, so two
// state directories on one engine never pick the same name. Names alone do
// not prove ownership; labels do.
func prefix(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "boxpool-" + hex.EncodeToString(sum[:6]) + "-"
}

// layout is the host paths of one sandbox: dir is 0700, so only the daemon's
// user reaches the agent socket in agent, which is bind-mounted into the
// container and writable by its root.
type layout struct {
	dir, agent, sock string
}

func sandboxLayout(root, id string) layout {
	dir := filepath.Join(root, id)
	agent := filepath.Join(dir, "agent")
	return layout{dir: dir, agent: agent, sock: filepath.Join(agent, "agent.sock")}
}

// splitImage splits an image reference into the repository and the tag or
// digest to pull. A reference without either is refused: it would mean the
// mutable latest tag.
func splitImage(ref string) (repo, tag string, err error) {
	if r, digest, ok := strings.Cut(ref, "@"); ok {
		// A tag next to the digest is ignored by the engine; drop it.
		if i := strings.LastIndex(r, ":"); i > strings.LastIndex(r, "/") {
			r = r[:i]
		}
		repo, tag = r, digest
	} else if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	if repo == "" || tag == "" {
		return "", "", fmt.Errorf("image %q: pin a tag or a digest", ref)
	}
	return repo, tag, nil
}

// mount is a HostConfig.Mounts entry.
type mount struct {
	Type   string
	Source string
	Target string
}

type hostConfig struct {
	NetworkMode    string
	Mounts         []mount
	Tmpfs          map[string]string
	CapDrop        []string
	CapAdd         []string
	SecurityOpt    []string
	ReadonlyRootfs bool
	Privileged     bool
	NanoCPUs       int64 `json:"NanoCpus"`
	Memory         int64
	MemorySwap     int64
	PidsLimit      int64
}

// containerConfig is the body of POST /containers/create.
type containerConfig struct {
	Image      string
	Cmd        []string
	WorkingDir string
	Labels     map[string]string
	HostConfig hostConfig
}

func labels(root, id string) map[string]string {
	return map[string]string{labelOwner: root, labelSandbox: id}
}

// newContainerConfig returns the hardened container for a sandbox: no
// capabilities, no privilege gain, a read-only root filesystem with tmpfs
// on /tmp, /var/tmp and /run, the volume vol on /root, the host's agent
// directory on /run/boxpool, its own network, and the Spec's CPU and memory
// as limits. No ports are published; Docker's default seccomp and AppArmor
// profiles stay on. Spec.DiskGB is not applied: a plain volume has no quota.
func newContainerConfig(root, id string, spec backend.Spec, image, name string, l layout) containerConfig {
	lb := labels(root, id)
	lb[labelName] = spec.Name
	mem := int64(spec.MemoryMB) << 20
	// tmpfs pages count against the memory limit; this bounds each one too.
	tmp := fmt.Sprintf("rw,nosuid,nodev,size=%dm,mode=1777", max(spec.MemoryMB/2, 1))
	return containerConfig{
		Image:      image,
		Cmd:        []string{"/bin/sh", "-c", agentCmd},
		WorkingDir: homeDir,
		Labels:     lb,
		HostConfig: hostConfig{
			NetworkMode: name,
			Mounts: []mount{
				{Type: "volume", Source: name, Target: homeDir},
				{Type: "bind", Source: l.agent, Target: agentDir},
			},
			Tmpfs: map[string]string{
				"/tmp":     tmp,
				"/var/tmp": tmp,
				"/run":     "rw,nosuid,nodev,noexec,size=16m,mode=0755",
			},
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{},
			SecurityOpt:    []string{"no-new-privileges:true"},
			ReadonlyRootfs: true,
			NanoCPUs:       int64(spec.CPUs) * 1e9,
			Memory:         mem,
			MemorySwap:     mem, // no swap
			PidsLimit:      pidsLimit,
		},
	}
}
