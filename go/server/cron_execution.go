/*
 *
 * Copyright 2021 The Vitess Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 * /
 */

package server

import (
	"fmt"
	"github.com/vitessio/arewefastyet/go/exec"
	"time"
)

func (s *Server) executeSingle(config string, identifier executionIdentifier) (err error) {
	var e *exec.Exec
	defer func() {
		if e != nil {
			if err != nil {
				err = fmt.Errorf("%v", err)
			}
			if errSuccess := e.Success(); errSuccess != nil {
				err = errSuccess
				return
			}
			slog.Info("Finished execution: UUID: [", e.UUID.String(), "], Git Ref: [", identifier.GitRef, "], Type: [", identifier.BenchmarkType, "]")
		}
	}()

	e, err = exec.NewExecWithConfig(config)
	if err != nil {
		slog.Error(err.Error())
		return err
	}
	e.Source = identifier.Source
	e.GitRef = identifier.GitRef
	e.VtgatePlannerVersion = identifier.PlannerVersion
	e.PullNB = identifier.PullNb

	slog.Info("Starting execution: UUID: [", e.UUID.String(), "], Git Ref: [", identifier.GitRef, "], Type: [", identifier.BenchmarkType, "]")
	err = e.Prepare()
	if err != nil {
		return fmt.Errorf(fmt.Sprintf("prepare step error: %v", err))
	}

	err = e.SetOutputToDefaultPath()
	if err != nil {
		return fmt.Errorf(fmt.Sprintf("prepare outputs step error: %v", err))
	}

	err = e.ExecuteWithTimeout(time.Hour * 2)
	if err != nil {
		return fmt.Errorf(fmt.Sprintf("execution step error: %v", err))
	}
	return nil
}

func (s *Server) executeElement(element *executionQueueElement) {
	if element.retry < 0 {
		if _, found := queue[element.identifier]; found {
			// removing the element from the queue since we are done with it
			mtx.Lock()
			delete(queue, element.identifier)
			mtx.Unlock()
		}
		decrementNumberOfOnGoingExecution()
		return
	}

	// execute with the given configuration file and exec identifier
	err := s.executeSingle(element.config, element.identifier)
	if err != nil {
		slog.Error(err.Error())

		// execution failed, we retry
		element.retry -= 1
		s.executeElement(element)
		return
	}

	go func() {
		s.compareElement(element)

		// removing the element from the queue since we are done with it
		mtx.Lock()
		delete(queue, element.identifier)
		mtx.Unlock()
	}()

	decrementNumberOfOnGoingExecution()
}

func (s *Server) compareElement(element *executionQueueElement) {
	done := 0
	for done != len(element.compareWith) {
		time.Sleep(1 * time.Second)
		for _, comparer := range element.compareWith {
			comparerUUID, err := exec.GetFinishedExecution(s.dbClient, comparer.GitRef, comparer.Source, comparer.BenchmarkType, comparer.PlannerVersion, comparer.PullNb)
			if err != nil {
				slog.Error(err)
				return
			}
			if comparerUUID != "" {
				err := s.sendNotificationForRegression(
					element.identifier.Source,
					comparer.Source,
					element.identifier.GitRef,
					comparer.GitRef,
					element.identifier.PlannerVersion,
					element.identifier.BenchmarkType,
					element.identifier.PullNb,
					element.notifyAlways,
				)
				if err != nil {
					slog.Error(err)
					return
				}
				done++
			}
		}
	}
}

func (s *Server) checkIfExecutionExists(identifier executionIdentifier) (bool, error) {
	checkStatus := []struct {
		status string
		today bool
	} {
		{status: exec.StatusFinished, today: false},
	}
	for _, status := range checkStatus {
		var exists bool
		var err error
		if identifier.BenchmarkType == "micro" {
			exists, err = exec.Exists(s.dbClient, identifier.GitRef, identifier.Source, identifier.BenchmarkType, status.status)
		} else {
			exists, err = exec.ExistsMacrobenchmark(s.dbClient, identifier.GitRef, identifier.Source, identifier.BenchmarkType, status.status, identifier.PlannerVersion)
		}
		if err != nil {
			slog.Error(err)
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) cronExecutionQueueWatcher() {
	for {
		time.Sleep(time.Second * 1)
		mtx.Lock()
		if currentCountExec >= maxConcurJob {
			mtx.Unlock()
			continue
		}
		for _, element := range queue {
			if !element.executing {
				currentCountExec++

				// setting this element to `executing = true`, so we do not execute it twice in the future
				element.executing = true
				go s.executeElement(element)
				break
			}
		}
		mtx.Unlock()
	}
}

func decrementNumberOfOnGoingExecution() {
	mtx.Lock()
	currentCountExec--
	mtx.Unlock()
}
