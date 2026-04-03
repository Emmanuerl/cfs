# Container Internals
### Understanding the Primitives Powering Container Runtimes

---

## 1. Introduction

### What is a container? (The popular definition)
- A lightweight, portable unit of software
- "It works on my machine" — solved
- An isolated environment that packages code and its dependencies
- Often confused with virtual machines — containers share the host kernel

### What a container *really* is
> *"A container is just a process that's been convinced it's alone on the machine."*

- At its core: a **Linux process** that is isolated and given its own filesystem
- No magic. No separate OS. Just the kernel doing what it has always done.
- Three primitives make this possible: **Cgroups**, **Namespaces**, and **Chroot**

### Does this mean all containers are Linux-based?
- On a Linux host — yes. Containers share the host kernel, so all binaries must be Linux-compatible
- The base image doesn't give you a kernel — it just gives you a **filesystem**. The kernel always comes from the host
- On macOS and Windows, Docker quietly runs a lightweight Linux VM in the background — you're still running Linux containers, just with a thin virtualization layer hidden underneath
- **The exception:** Windows has its own native container implementation using Windows kernel primitives (Job Objects, silos) — a completely separate implementation from what we'll discuss today

### How minimal can a container be?
- `ubuntu`, `alpine` — full Linux userspace
- `distroless` — just runtime dependencies, no shell
- `scratch` — a completely empty filesystem

With `scratch`, there is no shell, no libc, nothing. The kernel executes your binary directly:

```dockerfile
FROM scratch
COPY myapp /myapp
CMD ["/myapp"]
```

For this to work the binary must be **statically compiled** — all dependencies baked in at compile time. This is why Go became popular in the container world:

```bash
CGO_ENABLED=0 go build -o myapp .
```

> **The kernel always runs your binary** — the shell is just a messenger. When you type `./myapp`, the shell calls `execve()` and hands off to the kernel. You can see this with `strace ./myapp` — the very first syscall is `execve()`.

---

## 2. Cgroups (Control Groups)

### What are they?
- A Linux kernel feature for **organizing and limiting resource usage** of processes
- Answers the question: *"What stops one process from eating all the CPU, memory, or I/O?"*
- Every process on your system already belongs to a cgroup — even before you create one
- Check which cgroup your current shell is in:

```bash
cat /proc/self/cgroup
# 0::/user.slice/user-1000.slice/session-3.scope
```

### systemd's cgroup tree
Systemd automatically organizes all processes into a hierarchy at boot:

```
/sys/fs/cgroup/
├── system.slice      ← system daemons (dockerd, containerd, sshd, cron...)
├── user.slice        ← logged in users
│   └── user-1000.slice
│       └── session-3.scope   ← your terminal session lives here
└── init.scope        ← PID 1 (systemd itself)
```

- **`system.slice`** — processes that run independently of any user session
- **`user.slice`** — everything belonging to logged in users
- Docker containers land under `system.slice` — they're treated as system services, not user sessions

### Checking your cgroup version
```bash
stat -fc %T /sys/fs/cgroup
# cgroup2fs → V2
# tmpfs     → V1
```

### Resource Controllers
Each controller manages a specific resource type. See what's available on your system:

```bash
cat /sys/fs/cgroup/cgroup.controllers
# cpu cpuset io memory hugetlb pids rdma misc dmem
```

| Controller | What it controls |
|---|---|
| `cpu` | CPU time and scheduling weight |
| `cpuset` | Which CPU cores a process can use |
| `memory` | RAM and swap usage |
| `io` | Block device read/write bandwidth and IOPS |
| `pids` | Number of processes and threads |
| `hugetlb` | Huge page memory (used by DBs, JVMs) |
| `rdma` | InfiniBand/RDMA networking (HPC) |
| `misc` | Scalar resources that don't fit elsewhere |
| `dmem` | GPU/device memory (added for AI/ML workloads) |

### Cgroups V1 vs V2

**V1 — separate hierarchies per controller:**
```
/sys/fs/cgroup/
├── memory/mycontainer/memory.limit_in_bytes
├── cpu/mycontainer/cpu.shares
├── pids/mycontainer/pids.max
└── blkio/mycontainer/blkio.weight
```
A process could be in `memory/groupA` and `cpu/groupB` simultaneously — different controllers, different groups. This sounds flexible but caused real problems: conflicting policies, complex accounting, and inconsistent delegation.

**V2 — single unified hierarchy:**
```
/sys/fs/cgroup/mycontainer/
├── cgroup.procs    ← assign process here
├── memory.max
├── cpu.max
├── pids.max
└── io.max
```
A process belongs to **exactly one cgroup** and that cgroup controls all resources. One directory, all controllers in one place.

### The no-exceed rule
A child cgroup can never exceed its parent's limits — the kernel enforces this at write time:

```
/sys/fs/cgroup/mygroup/
├── memory.max  (512MB)
├── process_a/
│   └── memory.max  (256MB) ← valid
└── process_b/
    └── memory.max  (768MB) ← kernel rejects this
```

Limits can only get **narrower** as you go deeper, never wider.

### The no internal process rule
A cgroup cannot have both processes assigned to it AND have children simultaneously:

```
internal nodes  →  resource budgets, no processes
leaf nodes      →  actual processes, no children
```

This makes resource accounting straightforward — reading `memory.current` on a parent gives you the total usage of everything underneath it.

### How do we use them?
Before writing to controller files, enable them in the parent:

```bash
echo "+memory +pids +cpu" > /sys/fs/cgroup/cgroup.subtree_control
```

Then create and configure your cgroup:

```bash
mkdir /sys/fs/cgroup/mycontainer
echo "52428800" > /sys/fs/cgroup/mycontainer/memory.max
echo "20"       > /sys/fs/cgroup/mycontainer/pids.max
echo $$ > /sys/fs/cgroup/mycontainer/cgroup.procs
```

Or in Go:

```go
os.MkdirAll("/sys/fs/cgroup/mycontainer", 0755)
os.WriteFile("/sys/fs/cgroup/mycontainer/memory.max", []byte("52428800"), 0)
os.WriteFile("/sys/fs/cgroup/mycontainer/pids.max", []byte("20"), 0)
os.WriteFile("/sys/fs/cgroup/mycontainer/cgroup.procs", []byte(fmt.Sprint(os.Getpid())), 0)
```

> **Note:** `os.Mkdir` only creates the final directory — use `os.MkdirAll` for the full path, like `mkdir -p`. Also note the `0` permission flag when writing to existing cgroup files — the kernel owns their permissions (`0644`), your flag is ignored on existing files anyway.

### The fork bomb demo
```bash
:() { : | : & }; :
```
This recursively spawns two copies of itself until the system runs out of PIDs. Inside a container with `pids.max 20` it dies harmlessly. Without cgroups it takes down the host. This is exactly why `pids.max` exists.

> **Gotcha for Go programs:** `pids.max` counts both processes AND threads. The Go runtime spins up several OS threads before your `main()` even runs (GC, scheduler, etc.). Set `pids.max` too low and your container crashes not because you spawned too many processes, but because the Go runtime needs those threads just to function.

### Docker's cgroup flags map directly to cgroup files:
```
--memory 512m   →  memory.max
--cpus 1.5      →  cpu.max
--pids-limit 50 →  pids.max
```
Docker is literally just writing values to these files. Nothing more.

---

## 3. Namespaces

### What are they?
- A Linux kernel feature that **partitions kernel resources** so processes have isolated views of the system
- Answers the question: *"What stops a process from seeing other processes, network interfaces, or filesystems?"*
- Each process belongs to exactly one namespace of each type

### Types of Namespaces

| Namespace | Flag | Isolates |
|---|---|---|
| PID | `CLONE_NEWPID` | Process IDs |
| Network | `CLONE_NEWNET` | Network interfaces, routes, firewall rules |
| Mount | `CLONE_NEWNS` | Filesystem mount points |
| UTS | `CLONE_NEWUTS` | Hostname and domain name |
| IPC | `CLONE_NEWIPC` | Interprocess communication |
| User | `CLONE_NEWUSER` | User and group IDs (enables rootless containers) |
| Cgroup | `CLONE_NEWCGROUP` | Cgroup root directory |
| Time | `CLONE_NEWTIME` | System clocks (Linux 5.6+) |

> You can inspect a process's namespaces at `/proc/self/ns` — each file is a symlink to the namespace the process belongs to.

### How do we use them?

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
}
```

Or experiment without writing code:
```bash
unshare --uts /bin/bash
hostname mycontainer  # only changed inside the namespace
```

### Namespaces compose — PID + Mount together
`CLONE_NEWPID` alone is not enough. Without `CLONE_NEWNS`, userspace tools like `ps` still read the host's `/proc` and show every process on the machine.

The fix — remount `/proc` inside a new mount namespace:

```go
syscall.Mount("proc", "/proc", "proc", 0, "")
```

**Without `CLONE_NEWNS`** this remounts `/proc` on the **host** — breaking every other process on your machine. Always use mount and PID namespaces together.

> **On modern kernels:** plain `ps` inside a PID namespace without remounting `/proc` shows *more* processes than on the host — your new processes plus all host processes. This is `ps` reading the host's unremounted `/proc` and losing its TTY-based filter. The 2018 Liz Rice demo behaved differently because kernel and `ps` versions have changed significantly since then.

### TTY — a quick note
TTY (teletype) is the terminal interface a process is connected to. Plain `ps` with no flags filters by your TTY — showing only processes in your terminal session. When the PID namespace disrupts how processes map to TTYs in `/proc`, `ps` loses its filter and dumps everything. Fix `/proc` and `ps` works correctly again.

### Rootless containers and User namespaces
`CLONE_NEWUSER` maps your unprivileged user to UID 0 inside the namespace — "fake root" that has privileges inside but not on the host. This is what Podman's rootless mode uses. A compromised container running as root inside a user namespace is still just an unprivileged user on the host.

---

## 4. Chroot

### A review of the Linux filesystem
- Linux uses a single hierarchical tree rooted at `/`
- Everything is a file — devices, sockets, processes (via `/proc`)
- A process inherits the root filesystem of its parent

### Why we need a new root filesystem
- By default a process can navigate the entire host filesystem
- `chroot` changes the apparent root directory for a process:

```bash
mkdir -p /tmp/myroot/{bin,lib,lib64}
cp /bin/bash /tmp/myroot/bin/
chroot /tmp/myroot /bin/bash
```

### Why containers go beyond chroot
- `chroot` jails are escapable — a privileged process can break out
- It doesn't handle mount points cleanly
- Modern runtimes use **`pivot_root`** + **mount namespaces** — a much stronger boundary
- `chroot` is the ancestor. `pivot_root` + mount namespaces is how containers actually do it.

### chroot and exec — order matters
`chroot` only affects **kernel path resolution**. Go's `exec.Command` uses `LookPath` internally which resolves binaries through Go's own file descriptors — set up before `chroot` ran — so it still sees the host filesystem.

`syscall.Exec` hands the path **directly to the kernel**, which resolves it after `chroot` has already taken effect:

```go
// Wrong — Go resolves /bin/bash on the host before chroot matters
cmd := exec.Command("/bin/bash")

// Correct — kernel resolves /bin/bash inside the new root
syscall.Exec(os.Args[2], os.Args[2:], os.Environ())
```

> `syscall.Exec` also **replaces** the current process rather than spawning a child. This means your container process becomes PID 1 inside the namespace — exactly what Docker does with your entrypoint. Using `exec.Command` leaves a wrapper process sitting at PID 1 with your actual process as PID 2.

---

## 5. Why your Go runtime can't just fork()

When your Go program starts, the runtime has already spun up multiple OS threads before `main()` runs:

```
your Go program
├── thread 1  → main goroutine
├── thread 2  → garbage collector
├── thread 3  → goroutine scheduler
└── thread 4  → other runtime internals
```

`fork()` only clones the calling thread. The other threads vanish in the child — but their locks and state remain in memory:

```
child after fork
└── main goroutine (cloned)
    └── tries to allocate memory
        → needs GC thread (gone)
        → malloc lock held by dead thread
        → deadlock
```

This is why Go has no `os.Fork`. The safe alternative is always **fork + exec together** — `exec.Command` and `syscall.ForkExec` both do this. The exec immediately replaces the broken child with a fresh process before the runtime inconsistency causes problems.

This is also why the `/proc/self/exe` trick exists in your code — re-executing the same binary with a different `os.Args` branch is the correct Go pattern for spawning a process with different behavior:

```go
// run() — sets up namespaces, re-executes self as "child"
cmd := exec.Command("/proc/self/exe", append([]string{"child"}, os.Args[2:]...)...)

// child() — does cgroup/chroot/mount setup, then execs the actual command
syscall.Exec(os.Args[2], os.Args[2:], os.Environ())
```

---

## 5.5 Networking — How Containers Talk to the World

### The building blocks
Container networking is built on three Linux primitives working together:

```
network namespace  →  gives the container an isolated network stack
veth pairs         →  virtual ethernet cable connecting container to host
bridge (docker0)   →  virtual switch connecting all containers together
iptables           →  handles routing, NAT, and port mapping
```

### What happens when Docker installs
Docker creates a bridge interface `docker0` once at install time — a virtual switch that all containers on the default network plug into:

```
internet
    ↑
wlp0s20f3 (your wifi — 192.168.1.11)
    ↑
  NAT (iptables)
    ↑
docker0 (bridge — 172.17.0.1)
   /        \
veth0       veth1
  ↕            ↕
container 1  container 2
172.17.0.2   172.17.0.3
```

Custom networks (`docker network create`) get their own separate bridge — `br-bd4be9b333d8` for example. Containers on different bridges cannot talk to each other unless explicitly connected. Separate bridges, separate switches, separate isolation.

### For every new container Docker:
```
1. Creates a new network namespace     →  CLONE_NEWNET
2. Creates a veth pair                 →  ip link add veth0 type veth peer name veth1
3. Moves one end into the namespace    →  ip link set veth1 netns <container_pid>
4. Attaches other end to the bridge    →  ip link set veth0 master docker0
5. Assigns IP to the namespace end     →  ip addr add 172.17.0.2/16 dev veth1
6. Brings interfaces up                →  ip link set veth0 up / ip link set veth1 up
7. Sets default route in namespace     →  ip route add default via 172.17.0.1
```

Inside the container all it sees is:
```
lo    (127.0.0.1)
eth0  (172.17.0.2)   ← actually veth1, renamed
```

The bridge, the host veth end, the wifi card — all completely invisible to the container. Network namespace at work.

### Port mapping — how localhost:8080 reaches a container
When you run `docker run -p 8080:80 nginx`, Docker adds an iptables NAT rule:

```bash
# what Docker adds under the hood
iptables -t nat -A DOCKER -p tcp --dport 8080 -j DNAT --to-destination 172.17.0.2:80
```

The full journey of a request:
```
browser → localhost:8080
  → iptables DNAT rewrites destination to 172.17.0.2:80
  → packet crosses docker0 bridge
  → arrives via veth inside container namespace
  → nginx receives it on port 80
  → response follows same path back
```

You can inspect these rules yourself:
```bash
sudo iptables -t nat -L -n
```

> **Why another machine can't reach your container by default:** the iptables rule only applies to the host's own interfaces. To accept external traffic you need to bind to `0.0.0.0` explicitly. This is also why ports below 1024 require root — adding iptables rules is a privileged operation.

### The unified picture
The same pattern holds for networking as for everything else:

```
primitive            →  creates isolation
runtime plumbing     →  makes isolation useful

network namespace    →  container has no network interfaces
veth + bridge        →  container gets real connectivity and an IP
iptables NAT         →  host ports map cleanly into the container
```

---

## 6. The Assembly Line
> *How a runtime stitches these primitives together*

When you run `docker run`:

```
1. Create Namespaces     →  isolate the process's view of the world
         ↓
2. Set up Cgroups        →  constrain what resources it can consume
         ↓
3. Prepare the rootfs    →  unpack the image layers
         ↓
4. pivot_root / chroot   →  drop the process into the new filesystem
         ↓
5. exec the entrypoint   →  your process starts as PID 1, thinking it owns the machine
```

---

## 7. Demo Time!

> *Using a custom-built container runtime to bring it all together*

### What the demo will show
- Re-executing via `/proc/self/exe` — the correct Go fork pattern
- Spawning a process in new namespaces using `clone` flags
- Attaching it to a cgroup with resource limits
- Remounting `/proc` inside a new mount namespace
- `chroot` + `syscall.Exec` to drop into an isolated filesystem with the command as PID 1

### What to watch for
- **If you're newer to this:** Notice how the process loses visibility of the host's processes and filesystem — that's namespaces and chroot at work
- **If you've worked with runc before:** Pay attention to how the namespace flags map to the `config.json` spec fields, and how the cgroup setup maps to Docker's resource flags

---

## 8. Where These Primitives Live in the Real World

| Tool | Role | Primitives used |
|---|---|---|
| **runc** | OCI runtime — actually creates containers | All three: cgroups, namespaces, pivot_root |
| **containerd** | Lifecycle manager — pulls images, manages state | Delegates to runc for container creation |
| **Docker** | Developer-facing CLI and daemon | Delegates to containerd → runc |
| **Podman** | Daemonless Docker alternative | Uses runc/crun, strong rootless via user namespaces |
| **systemd-nspawn** | Lightweight container runner | Namespaces + cgroups via systemd's hierarchy |
| **Kubernetes** | Orchestration | CRI → containerd/CRI-O → runc |

### The call chain
```
docker run
  └── containerd
        └── containerd-shim
              └── runc
                    └── clone() + cgroups + pivot_root
```

> `containerd-shim` exists so that if `containerd` restarts, your container doesn't die — the shim keeps it alive independently.

---

## Key Takeaways

1. **A container is a process** — not a VM, not a sandbox, not magic
2. **Cgroups** control *how much* of the machine a process can use
3. **Namespaces** control *what* a process can see
4. **Chroot / pivot_root** controls *where* in the filesystem it lives
5. **Namespaces compose** — PID namespace alone isn't enough, mount namespace must come with it
6. **`syscall.Exec` replaces, `exec.Command` spawns** — use the right one or your PID 1 story breaks
7. Every container runtime you've used is ultimately calling these same kernel primitives

---

## Further Reading & References

- [Linux man page — clone(2)](https://man7.org/linux/man-pages/man2/clone.2.html)
- [Linux man page — cgroups(7)](https://man7.org/linux/man-pages/man7/cgroups.7.html)
- [Linux man page — namespaces(7)](https://man7.org/linux/man-pages/man7/namespaces.7.html)
- [Linux man page — chroot(2)](https://man7.org/linux/man-pages/man2/chroot.2.html)
- [OCI Runtime Spec](https://github.com/opencontainers/runtime-spec)
- [runc source code](https://github.com/opencontainers/runc)
- *"Containers from Scratch"* — Liz Rice (2018, the talk this demo is based on)
