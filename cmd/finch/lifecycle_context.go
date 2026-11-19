// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import "context"

// signalBackgroundContext is the root context for vm commands: it starts from
// context.Background and is turned into a signal-cancelling context by the
// lifecycle package inside each action.
func signalBackgroundContext() context.Context { return context.Background() }
