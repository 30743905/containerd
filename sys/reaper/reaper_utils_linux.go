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

package reaper

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// SetSubreaper sets the value i as the subreaper setting for the calling process
func SetSubreaper(i int) error {
	/**
	subreaper, 自linux3.4内核起有的一个系统调用，subreaper 由名字可得是一个子进程的收割者,
	意思就是通过PR_SET_CHILD_SUBREAPER这个系统调用便能把一个进程设置为祖先进程与init进程一样可以收养孤儿进程
	而子进程被收养的方式是先会被自己最近的祖先先收养。
	*/
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(i), 0, 0, 0)
}

// GetSubreaper returns the subreaper setting for the calling process
func GetSubreaper() (int, error) {
	var i uintptr

	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&i)), 0, 0, 0); err != nil {
		return -1, err
	}

	return int(i), nil
}
