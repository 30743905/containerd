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

package v2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	gruntime "runtime"

	"github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/protobuf"
	"github.com/containerd/containerd/protobuf/proto"
	"github.com/containerd/containerd/protobuf/types"
	"github.com/containerd/containerd/runtime"
	client "github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/log"
)

type shimBinaryConfig struct {
	runtime      string
	address      string
	ttrpcAddress string
	schedCore    bool
}

func shimBinary(bundle *Bundle, config shimBinaryConfig) *binary {
	return &binary{
		bundle:                 bundle,
		runtime:                config.runtime,
		containerdAddress:      config.address,
		containerdTTRPCAddress: config.ttrpcAddress,
		schedCore:              config.schedCore,
	}
}

type binary struct {
	runtime                string
	containerdAddress      string
	containerdTTRPCAddress string
	schedCore              bool
	bundle                 *Bundle
}

func (b *binary) Start(ctx context.Context, opts *types.Any, onClose func()) (_ *shim, err error) {
	args := []string{"-id", b.bundle.ID}
	switch log.GetLevel() {
	case log.DebugLevel, log.TraceLevel:
		args = append(args, "-debug")
	}
	args = append(args, "start")

	cmd, err := client.Command(
		ctx,
		&client.CommandConfig{
			Runtime:      b.runtime,
			Address:      b.containerdAddress,
			TTRPCAddress: b.containerdTTRPCAddress,
			Path:         b.bundle.Path,
			Opts:         opts,
			Args:         args,
			SchedCore:    b.schedCore,
		})
	log.G(ctx).Info("构建 exec shim调用------->", b.runtime, "###", b.bundle.Path, "###", opts, "###", args, "###", b.schedCore)
	if err != nil {
		return nil, err
	}
	// Windows needs a namespace when openShimLog
	ns, _ := namespaces.Namespace(ctx)
	shimCtx, cancelShimLog := context.WithCancel(namespaces.WithNamespace(context.Background(), ns))
	defer func() {
		if err != nil {
			cancelShimLog()
		}
	}()
	f, err := openShimLog(shimCtx, b.bundle, client.AnonDialer)
	if err != nil {
		return nil, fmt.Errorf("open shim log pipe: %w", err)
	}
	defer func() {
		if err != nil {
			f.Close()
		}
	}()
	// open the log pipe and block until the writer is ready
	// this helps with synchronization of the shim
	// copy the shim's logs to containerd's output
	go func() {
		defer f.Close()
		_, err := io.Copy(os.Stderr, f)
		// To prevent flood of error messages, the expected error
		// should be reset, like os.ErrClosed or os.ErrNotExist, which
		// depends on platform.
		err = checkCopyShimLogError(ctx, err)
		if err != nil {
			log.G(ctx).WithError(err).Error("copy shim log")
		}
	}()
	//exec执行真正是在这里执行
	log.G(ctx).Info("exec shim开始调用------->")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", out, err)
	}
	// exec调用containerd-shim解析标准输出获取返回值如：unix:///run/containerd/s/c1de1e37994956b2b4ee802a3556de1701fc1eeb4525bd013b71b44bec00ccfe
	response := bytes.TrimSpace(out)

	onCloseWithShimLog := func() {
		onClose()
		cancelShimLog()
		f.Close()
	}
	// Save runtime binary path for restore.
	// shim运行时写入到如下文件中：
	// /run/containerd/io.containerd.runtime.v2.task/moby/06d04a4c035f5a0a6b53a1ba7e51995564b1ffb78f4acd8bc4511cf08cf602d6/shim-binary-path
	if err := os.WriteFile(filepath.Join(b.bundle.Path, "shim-binary-path"), []byte(b.runtime), 0600); err != nil {
		return nil, err
	}

	//
	/**
	解析containerd-shim返回的结果，即containerd-shim创建的本地套接字，用于后续containerd和containerd-shim创建ttrpc通信

	unix:///run/containerd/s/87b824a4f57c0ec3ad871e365cb830d73f066099997821c737af36f30c536db4

	如：/var/run/containerd/s/fed46ec07ca1086999d44bdc22b1d107103b43cf681257acd0da6f613f87582a
	*/
	params, err := parseStartResponse(ctx, response)
	if err != nil {
		return nil, err
	}

	//和containerd-shim进程建立连接使用ttrpc协议通信
	conn, err := makeConnection(ctx, params, onCloseWithShimLog)
	if err != nil {
		return nil, err
	}

	return &shim{
		bundle: b.bundle,
		client: conn,
	}, nil
}

func (b *binary) Delete(ctx context.Context) (*runtime.Exit, error) {
	log.G(ctx).Info("cleaning up dead shim")

	// On Windows and FreeBSD, the current working directory of the shim should
	// not be the bundle path during the delete operation. Instead, we invoke
	// with the default work dir and forward the bundle path on the cmdline.
	// Windows cannot delete the current working directory while an executable
	// is in use with it. On FreeBSD, fork/exec can fail.
	var bundlePath string
	if gruntime.GOOS != "windows" && gruntime.GOOS != "freebsd" {
		bundlePath = b.bundle.Path
	}
	args := []string{
		"-id", b.bundle.ID,
		"-bundle", b.bundle.Path,
	}
	switch log.GetLevel() {
	case log.DebugLevel, log.TraceLevel:
		args = append(args, "-debug")
	}
	args = append(args, "delete")

	cmd, err := client.Command(ctx,
		&client.CommandConfig{
			Runtime:      b.runtime,
			Address:      b.containerdAddress,
			TTRPCAddress: b.containerdTTRPCAddress,
			Path:         bundlePath,
			Opts:         nil,
			Args:         args,
		})

	if err != nil {
		return nil, err
	}
	var (
		out  = bytes.NewBuffer(nil)
		errb = bytes.NewBuffer(nil)
	)
	cmd.Stdout = out
	cmd.Stderr = errb
	if err := cmd.Run(); err != nil {
		log.G(ctx).WithField("cmd", cmd).WithError(err).Error("failed to delete")
		return nil, fmt.Errorf("%s: %w", errb.String(), err)
	}
	s := errb.String()
	if s != "" {
		log.G(ctx).Warnf("cleanup warnings %s", s)
	}
	var response task.DeleteResponse
	if err := proto.Unmarshal(out.Bytes(), &response); err != nil {
		return nil, err
	}
	if err := b.bundle.Delete(); err != nil {
		return nil, err
	}
	return &runtime.Exit{
		Status:    response.ExitStatus,
		Timestamp: protobuf.FromTimestamp(response.ExitedAt),
		Pid:       response.Pid,
	}, nil
}
