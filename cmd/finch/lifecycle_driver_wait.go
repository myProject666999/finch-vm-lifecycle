// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import "time"

// killWaitDelay bounds how long the command waits for a cancelled child (and,
// on Unix, its process group) to exit before forcibly reaping it.
const killWaitDelay = 5 * time.Second
