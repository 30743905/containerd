//go:build !windows
// +build !windows

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

package integration

import (
	"fmt"

	exec "golang.org/x/sys/execabs"
)

func getVolumeHostPathOwnership(criRoot, containerID string) (string, error) {
	hostCmd := fmt.Sprintf("find %s/containers/%s/volumes/* | xargs stat -c %%U:%%G", criRoot, containerID)
	output, err := exec.Command("sh", "-c", hostCmd).CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(output), nil
}
