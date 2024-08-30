/*
   Copyright The containerd Authors.

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

package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	cgroupsv2 "github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/schedcore"
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containerd/containerd/runtime/v2/runc/options"
	"github.com/containerd/containerd/runtime/v2/shim"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// NewShimManager returns an implementation of the shim manager
// using runc
func NewShimManager(name string) shim.Manager {
	return &manager{
		name: name,
	}
}

// group labels specifies how the shim groups services.
// currently supports a runc.v2 specific .group label and the
// standard k8s pod label.  Order matters in this list
var groupLabels = []string{
	"io.containerd.runc.v2.group",
	"io.kubernetes.cri.sandbox-id",
}

// spec is a shallow version of [oci.Spec] containing only the
// fields we need for the hook. We use a shallow struct to reduce
// the overhead of unmarshaling.
type spec struct {
	// Annotations contains arbitrary metadata for the container.
	Annotations map[string]string `json:"annotations,omitempty"`
}

type manager struct {
	name string
}

func newCommand(ctx context.Context, id, containerdAddress, containerdTTRPCAddress string, debug bool) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	//os.Executable()获取当前程序运行路径
	//self=/usr/bin/containerd-shim-runc-v2
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	//os.Getwd()获取当前进程的工作目录
	//cwd=/run/containerd/io.containerd.runtime.v2.task/moby/fc386f64b2b0ebff000d9a306f461630ba9d66c3e6d2213ebd8d39ec84e62fde
	cwd, err := os.Getwd()
	log.G(ctx).Info("self----->", self)
	log.G(ctx).Info("cwd----->", cwd)
	if err != nil {
		return nil, err
	}
	/**

	-namespace moby
	-id fc386f64b2b0ebff000d9a306f461630ba9d66c3e6d2213ebd8d39ec84e62fde
	-address /run/containerd/containerd.sock
	*/
	args := []string{
		"-namespace", ns,
		"-id", id,
		"-address", containerdAddress,
	}
	log.G(ctx).Info("args----->", args)
	if debug {
		args = append(args, "-debug")
	}
	//在 Golang 中用于执行命令的库是 os/exec，exec.Command 函数返回一个 Cmd 对象
	//注意，并未执行
	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	return cmd, nil
}

func readSpec() (*spec, error) {
	f, err := os.Open(oci.ConfigFilename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var s spec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m manager) Name() string {
	return m.name
}

func (manager) Start(ctx context.Context, id string, opts shim.StartOpts) (_ string, retErr error) {
	log.G(ctx).Info("invoke----->111111111", opts)
	cmd, err := newCommand(ctx, id, opts.Address, opts.TTRPCAddress, opts.Debug)
	if err != nil {
		return "", err
	}
	grouping := id
	log.G(ctx).Info("grouping----->", grouping)
	spec, err := readSpec()
	if err != nil {
		return "", err
	}
	for _, group := range groupLabels {
		if groupID, ok := spec.Annotations[group]; ok {
			grouping = groupID
			break
		}
	}
	log.G(ctx).Info("grouping----->", grouping)
	//生成一个unix本地套接字地址
	address, err := shim.SocketAddress(ctx, opts.Address, grouping)
	log.G(ctx).Info("address----->", address)
	if err != nil {
		return "", err
	}
	//对上面生成的UNIX本地套接字监听
	socket, err := shim.NewSocket(address)
	if err != nil {
		// the only time where this would happen is if there is a bug and the socket
		// was not cleaned up in the cleanup method of the shim or we are using the
		// grouping functionality where the new process should be run with the same
		// shim as an existing container
		if !shim.SocketEaddrinuse(err) {
			return "", fmt.Errorf("create new shim socket: %w", err)
		}
		if shim.CanConnect(address) {
			if err := shim.WriteAddress("address", address); err != nil {
				return "", fmt.Errorf("write existing socket for shim: %w", err)
			}
			return address, nil
		}
		if err := shim.RemoveSocket(address); err != nil {
			return "", fmt.Errorf("remove pre-existing socket: %w", err)
		}
		if socket, err = shim.NewSocket(address); err != nil {
			return "", fmt.Errorf("try create new shim socket 2x: %w", err)
		}
	}
	defer func() {
		if retErr != nil {
			socket.Close()
			_ = shim.RemoveSocket(address)
		}
	}()

	// make sure that reexec shim-v2 binary use the value if need
	//向工作目录下address文件写入address地址，
	//unix:///run/containerd/s/408144bcd644db49ac98579bb32f029b36107d612b2999be46278ad0db19f83f/address
	if err := shim.WriteAddress("address", address); err != nil {
		return "", err
	}

	f, err := socket.File()
	if err != nil {
		return "", err
	}

	// ExtraFiles是空
	log.G(ctx).Info("cmd.ExtraFiles----->", len(cmd.ExtraFiles))
	//f.Name：unix:/run/containerd/s/3b706a9677a279e028a3c97083be1e2abf44add4e49c182beff18651824ab2d5->
	log.G(ctx).Info("f.Name----->", f.Name())
	/**

	ExtraFiles指定额外被新进程继承的已打开文件流，不包括标准输入、标准输出、标准错误输出。

	除了标准输入输出0,1,2三个文件外，你还可以将父进程的文件传给子进程，通过Cmd.ExtraFiles字段就可以。
	比较常用的一个场景就是graceful restart,新的进程继承了老的进程监听的net.Listener,这样网络连接就不需要关闭重打开了。

	file := netListener.File() // this returns a Dup()
	path := "/path/to/executable"
	args := []string{
	    "-graceful"}
	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{file}
	err := cmd.Start()
	if err != nil {
	    log.Fatalf("gracefulRestart: Failed to launch, error: %v", err)
	}


	*/
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	goruntime.LockOSThread()
	log.G(ctx).Info("SCHED_CORE----->", os.Getenv("SCHED_CORE"))
	if os.Getenv("SCHED_CORE") != "" {
		if err := schedcore.Create(schedcore.ProcessGroup); err != nil {
			return "", fmt.Errorf("enable sched core support: %w", err)
		}
	}
	log.G(ctx).Info("begin----->")
	if err := cmd.Start(); err != nil {
		f.Close()
		return "", err
	}
	log.G(ctx).Info("end----->")
	goruntime.UnlockOSThread()

	defer func() {
		if retErr != nil {
			cmd.Process.Kill()
		}
	}()
	// make sure to wait after start
	go cmd.Wait()

	if opts, err := shim.ReadRuntimeOptions[*options.Options](os.Stdin); err == nil {
		// binary_name:\"runc\" root:\"/var/run/docker/runtime-runc\"
		log.G(ctx).Info("opts----->", opts)
		log.G(ctx).Info("ShimCgroup----->", opts.ShimCgroup)
		if opts.ShimCgroup != "" { //ShimCgroup空
			if cgroups.Mode() == cgroups.Unified {
				log.G(ctx).Info("if----->")
				cg, err := cgroupsv2.Load(opts.ShimCgroup)
				if err != nil {
					return "", fmt.Errorf("failed to load cgroup %s: %w", opts.ShimCgroup, err)
				}
				if err := cg.AddProc(uint64(cmd.Process.Pid)); err != nil {
					return "", fmt.Errorf("failed to join cgroup %s: %w", opts.ShimCgroup, err)
				}
			} else {
				log.G(ctx).Info("else----->")
				cg, err := cgroup1.Load(cgroup1.StaticPath(opts.ShimCgroup))
				if err != nil {
					return "", fmt.Errorf("failed to load cgroup %s: %w", opts.ShimCgroup, err)
				}
				if err := cg.AddProc(uint64(cmd.Process.Pid)); err != nil {
					return "", fmt.Errorf("failed to join cgroup %s: %w", opts.ShimCgroup, err)
				}
			}
		}
	}

	if err := shim.AdjustOOMScore(cmd.Process.Pid); err != nil {
		return "", fmt.Errorf("failed to adjust OOM score for shim: %w", err)
	}
	log.G(ctx).Info("address----->", address)
	return address, nil
}

func (manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return shim.StopStatus{}, err
	}

	path := filepath.Join(filepath.Dir(cwd), id)
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return shim.StopStatus{}, err
	}
	runtime, err := runc.ReadRuntime(path)
	if err != nil {
		return shim.StopStatus{}, err
	}
	opts, err := runc.ReadOptions(path)
	if err != nil {
		return shim.StopStatus{}, err
	}
	root := process.RuncRoot
	if opts != nil && opts.Root != "" {
		root = opts.Root
	}

	r := process.NewRunc(root, path, ns, runtime, false)
	if err := r.Delete(ctx, id, &runcC.DeleteOpts{
		Force: true,
	}); err != nil {
		log.G(ctx).WithError(err).Warn("failed to remove runc container")
	}
	if err := mount.UnmountRecursive(filepath.Join(path, "rootfs"), 0); err != nil {
		log.G(ctx).WithError(err).Warn("failed to cleanup rootfs mount")
	}
	pid, err := runcC.ReadPidFile(filepath.Join(path, process.InitPidFile))
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to read init pid file")
	}
	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + int(unix.SIGKILL),
		Pid:        pid,
	}, nil
}
