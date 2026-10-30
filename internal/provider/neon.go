// SPDX-License-Identifier: Apache-2.0

package provider

import "fmt"

// NewNeon returns the Neon provider.
func NewNeon(opts Options) (Provider, error) {
	return nil, fmt.Errorf("the neon provider is not available in this build")
}
