// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import "os"

// currentPID returns the process id written into operation records. It is a
// function rather than a direct os.Getpid call so tests can simulate different
// holders by constructing records explicitly.
func currentPID() int { return os.Getpid() }
