// Copyright 2024 PingCAP, Inc.
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

package integrationtests

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/tidb/pkg/disttask/framework/handle"
	"github.com/pingcap/tidb/pkg/disttask/framework/proto"
	"github.com/pingcap/tidb/pkg/disttask/framework/testutil"
	"github.com/pingcap/tidb/pkg/testkit/testfailpoint"
	"github.com/stretchr/testify/require"
)

func TestModifyTaskConcurrency(t *testing.T) {
	c := testutil.NewTestDXFContext(t, 1, 16, true)
	schedulerExt := testutil.GetMockSchedulerExt(c.MockCtrl, testutil.SchedulerInfo{
		AllErrorRetryable: true,
		StepInfos: []testutil.StepInfo{
			{Step: proto.StepOne, SubtaskCnt: 1},
			{Step: proto.StepTwo, SubtaskCnt: 1},
		},
	})
	subtaskCh := make(chan struct{})
	registerExampleTask(t, c.MockCtrl, schedulerExt, c.TestContext,
		func(ctx context.Context, subtask *proto.Subtask) error {
			select {
			case <-subtaskCh:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		},
	)

	t.Run("modify pending task concurrency", func(t *testing.T) {
		var once sync.Once
		modifySyncCh := make(chan struct{})
		var theTask *proto.Task
		testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/disttask/framework/scheduler/beforeGetSchedulableTasks", func() {
			once.Do(func() {
				task, err := handle.SubmitTask(c.Ctx, "k1", proto.TaskTypeExample, 3, "", nil)
				require.NoError(t, err)
				require.Equal(t, 3, task.Concurrency)
				require.NoError(t, c.TaskMgr.ModifyTaskByID(c.Ctx, task.ID, &proto.ModifyParam{
					PrevState: proto.TaskStatePending,
					Modifications: []proto.Modification{
						{Type: proto.ModifyConcurrency, To: 7},
					},
				}))
				theTask = task
				gotTask, err := c.TaskMgr.GetTaskBaseByID(c.Ctx, theTask.ID)
				require.NoError(t, err)
				require.Equal(t, proto.TaskStateModifying, gotTask.State)
				require.Equal(t, 3, gotTask.Concurrency)
				<-modifySyncCh
			})
		})
		modifySyncCh <- struct{}{}
		// finish subtasks
		subtaskCh <- struct{}{}
		subtaskCh <- struct{}{}
		task2Base := testutil.WaitTaskDone(c.Ctx, t, theTask.Key)
		require.Equal(t, proto.TaskStateSucceed, task2Base.State)
		checkSubtaskConcurrency(t, c, theTask.ID, map[proto.Step]int{
			proto.StepOne: 7,
			proto.StepTwo: 7,
		})
	})

	t.Run("modify running task concurrency at step two", func(t *testing.T) {
		var once sync.Once
		modifySyncCh := make(chan struct{})
		testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/disttask/framework/scheduler/beforeRefreshTask", func(task *proto.Task) {
			if task.State != proto.TaskStateRunning && task.Step != proto.StepTwo {
				return
			}
			once.Do(func() {
				require.NoError(t, c.TaskMgr.ModifyTaskByID(c.Ctx, task.ID, &proto.ModifyParam{
					PrevState: proto.TaskStateRunning,
					Modifications: []proto.Modification{
						{Type: proto.ModifyConcurrency, To: 7},
					},
				}))
				<-modifySyncCh
			})
		})
		task, err := handle.SubmitTask(c.Ctx, "k2", proto.TaskTypeExample, 3, "", nil)
		require.NoError(t, err)
		require.Equal(t, 3, task.Concurrency)
		// finish StepOne
		subtaskCh <- struct{}{}
		// wait task move to 'modifying' state
		modifySyncCh <- struct{}{}
		// wait task move back to 'running' state
		require.Eventually(t, func() bool {
			gotTask, err2 := c.TaskMgr.GetTaskByID(c.Ctx, task.ID)
			require.NoError(t, err2)
			return gotTask.State == proto.TaskStateRunning
		}, 10*time.Second, 100*time.Millisecond)
		// finish StepTwo
		subtaskCh <- struct{}{}
		task2Base := testutil.WaitTaskDone(c.Ctx, t, task.Key)
		require.Equal(t, proto.TaskStateSucceed, task2Base.State)
		checkSubtaskConcurrency(t, c, task.ID, map[proto.Step]int{
			proto.StepOne: 3,
			proto.StepTwo: 7,
		})
	})

	t.Run("modify paused task concurrency", func(t *testing.T) {
		var once sync.Once
		syncCh := make(chan struct{})
		var theTask *proto.Task
		testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/disttask/framework/scheduler/beforeGetSchedulableTasks", func() {
			once.Do(func() {
				task, err := handle.SubmitTask(c.Ctx, "k3", proto.TaskTypeExample, 3, "", nil)
				require.NoError(t, err)
				require.Equal(t, 3, task.Concurrency)
				found, err := c.TaskMgr.PauseTask(c.Ctx, task.Key)
				require.NoError(t, err)
				require.True(t, found)
				theTask = task
				<-syncCh
			})
		})
		syncCh <- struct{}{}
		taskBase := testutil.WaitTaskDoneOrPaused(c.Ctx, t, theTask.Key)
		require.Equal(t, proto.TaskStatePaused, taskBase.State)
		require.NoError(t, c.TaskMgr.ModifyTaskByID(c.Ctx, theTask.ID, &proto.ModifyParam{
			PrevState: proto.TaskStatePaused,
			Modifications: []proto.Modification{
				{Type: proto.ModifyConcurrency, To: 7},
			},
		}))
		taskBase = testutil.WaitTaskDoneOrPaused(c.Ctx, t, theTask.Key)
		require.Equal(t, proto.TaskStatePaused, taskBase.State)
		found, err := c.TaskMgr.ResumeTask(c.Ctx, theTask.Key)
		require.NoError(t, err)
		require.True(t, found)
		// finish subtasks
		subtaskCh <- struct{}{}
		subtaskCh <- struct{}{}
		task2Base := testutil.WaitTaskDone(c.Ctx, t, theTask.Key)
		require.Equal(t, proto.TaskStateSucceed, task2Base.State)
		checkSubtaskConcurrency(t, c, theTask.ID, map[proto.Step]int{
			proto.StepOne: 7,
			proto.StepTwo: 7,
		})
	})

	t.Run("modify pending task concurrency, but other owner already done it", func(t *testing.T) {
		var once sync.Once
		modifySyncCh := make(chan struct{})
		var theTask *proto.Task
		testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/disttask/framework/scheduler/beforeGetSchedulableTasks", func() {
			once.Do(func() {
				task, err := handle.SubmitTask(c.Ctx, "k4", proto.TaskTypeExample, 3, "", nil)
				require.NoError(t, err)
				require.Equal(t, 3, task.Concurrency)
				require.NoError(t, c.TaskMgr.ModifyTaskByID(c.Ctx, task.ID, &proto.ModifyParam{
					PrevState: proto.TaskStatePending,
					Modifications: []proto.Modification{
						{Type: proto.ModifyConcurrency, To: 7},
					},
				}))
				theTask = task
				gotTask, err := c.TaskMgr.GetTaskBaseByID(c.Ctx, theTask.ID)
				require.NoError(t, err)
				require.Equal(t, proto.TaskStateModifying, gotTask.State)
				require.Equal(t, 3, gotTask.Concurrency)
			})
		})
		var onceForRefresh sync.Once
		testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/disttask/framework/scheduler/afterRefreshTask",
			func(task *proto.Task) {
				onceForRefresh.Do(func() {
					require.Equal(t, proto.TaskStateModifying, task.State)
					taskClone := *task
					taskClone.Concurrency = 7
					require.NoError(t, c.TaskMgr.ModifiedTask(c.Ctx, &taskClone))
					gotTask, err := c.TaskMgr.GetTaskBaseByID(c.Ctx, task.ID)
					require.NoError(t, err)
					require.Equal(t, proto.TaskStatePending, gotTask.State)
					<-modifySyncCh
				})
			},
		)
		modifySyncCh <- struct{}{}
		// finish subtasks
		subtaskCh <- struct{}{}
		subtaskCh <- struct{}{}
		task2Base := testutil.WaitTaskDone(c.Ctx, t, theTask.Key)
		require.Equal(t, proto.TaskStateSucceed, task2Base.State)
		checkSubtaskConcurrency(t, c, theTask.ID, map[proto.Step]int{
			proto.StepOne: 7,
			proto.StepTwo: 7,
		})
	})
}

func checkSubtaskConcurrency(t *testing.T, c *testutil.TestDXFContext, taskID int64, expectedStepCon map[proto.Step]int) {
	for step, con := range expectedStepCon {
		subtasks, err := c.TaskMgr.GetSubtasksWithHistory(c.Ctx, taskID, step)
		require.NoError(t, err)
		require.Len(t, subtasks, 1)
		require.Equal(t, con, subtasks[0].Concurrency)
	}
}
