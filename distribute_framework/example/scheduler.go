// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package example

import (
	"context"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/distribute_framework/proto"
	"github.com/pingcap/tidb/distribute_framework/scheduler"
)

type ExampleStepOneScheduler struct {
	task *proto.Task
}

type ExampleStepTwoScheduler struct {
	task *proto.Task
}

func (s *ExampleStepOneScheduler) InitSubtaskExecEnv(ctx context.Context) error { return nil }

func (s *ExampleStepTwoScheduler) CleanupSubtaskExecEnv(ctx context.Context) error { return nil }

func (s *ExampleStepOneScheduler) SplitSubtasks(subtasks []*proto.Subtask) []*proto.Subtask {
	return subtasks
}

func init() {
	scheduler.RegisterSubtaskExectorConstructor(
		proto.TaskTypeExample,
		// The order of the subtask executors is the same as the order of the subtasks.
		func(subtask *proto.Subtask, step proto.TaskStep) (scheduler.SubtaskExecutor, error) {
			switch step {
			case stepOne:
				return &ExampleStepOneSubtaskExecutor{subtask: subtask}, nil
			case stepTwo:
				return &ExampleStepTwoSubtaskExecutor{subtask: subtask}, nil
			}
			return nil, errors.Errorf("unknown step %d", step)
		},
	)
}
