// This entire snippet is heavily inspired by Liz Rice's talk on "Containers from Scratch"
// and is arguably the easiest intro to container internals.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"syscall"
)

func main() {
	switch os.Args[1] {
	case "run":
		run()
	case "child":
		child()
	default:
		panic("bad command")
	}
}

// run creates a new process with the namespaces set
func run() {
	fmt.Printf("Invoking new process for %v as %d\n", os.Args[2:], os.Getpid())
	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, os.Args[2:]...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:   syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		Unshareflags: syscall.CLONE_NEWNS,
	}
	cmd.Run()
}

// child runs the specified command
func child() {
	fmt.Printf("Running %v as %d\n", os.Args[2:], os.Getpid())
	cg()
	syscall.Sethostname([]byte("container"))
	syscall.Chroot("/home/chukwuemeka/Desktop/tinkering/liz-rice-go-containers/ubuntu")
	syscall.Chdir("/")
	syscall.Mount("proc", "/proc", "proc", 0, "")

	// log.Fatal(syscall.Exec(os.Args[2], os.Args[3:], os.Environ()))
	cmd := exec.Command(os.Args[2], os.Args[3:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
	syscall.Unmount("/proc", 0) // unmount the proc fs we just mounted
}

func cg() {
	cgDir := "/sys/fs/cgroup/sample-group"
	err := os.Mkdir(cgDir, 0755)
	if err != nil && !os.IsExist(err) {
		panic(err)
	}

	must(os.WriteFile(path.Join(cgDir, "pids.max"), []byte("20"), 0700))
	must(os.WriteFile(path.Join(cgDir, "cgroup.procs"), []byte(fmt.Sprint(os.Getpid())), 0700))

}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
