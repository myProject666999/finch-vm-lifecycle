// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import "time"

// Clock abstracts time so takeover logic can be tested without real sleeps.
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock.
type realClock struct{}

// Now implements Clock.
func (realClock) Now() time.Time { return time.Now().UTC() }
