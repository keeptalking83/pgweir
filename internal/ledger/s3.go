// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"fmt"
)

// NewS3Sink returns a sink for s3://bucket/prefix.
func NewS3Sink(ctx context.Context, rawURL string) (Sink, error) {
	return nil, fmt.Errorf("S3 export is not available in this build")
}
