/*
Real-time Online/Offline Charging System (OCS) for Telecom & ISP environments
Copyright (C) ITsysCOM GmbH

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>
*/
package scheduler

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cgrates/cgrates/engine"
	"github.com/cgrates/cgrates/utils"
)

type Scheduler struct {
	sync.RWMutex
	queue                           engine.ActionTimingPriorityList
	timer                           *time.Timer
	restartLoop                     chan bool
	storage                         engine.RatingStorage
	schedulerStarted                bool
	actStatsInterval                time.Duration                 // How long time to keep the stats in memory
	actSucessChan, actFailedChan    chan *engine.Action           // ActionPlan will pass actions via these channels
	aSMux, aFMux                    sync.RWMutex                  // protect schedStats
	actSuccessStats, actFailedStats map[string]map[time.Time]bool // keep here stats regarding executed actions, map[actionType]map[execTime]bool
}

func NewScheduler(storage engine.RatingStorage) *Scheduler {
	s := &Scheduler{
		restartLoop: make(chan bool),
		storage:     storage,
	}
	s.Reload()
	return s
}

func (s *Scheduler) updateActStats(act *engine.Action, isFailed bool) {
	mux := s.aSMux
	statsMp := s.actSuccessStats
	if isFailed {
		mux = s.aFMux
		statsMp = s.actFailedStats
	}
	now := time.Now()
	mux.Lock()
	for aType := range statsMp {
		for t := range statsMp[aType] {
			if now.Sub(t) > s.actStatsInterval {
				delete(statsMp[aType], t)
				if len(statsMp[aType]) == 0 {
					delete(statsMp, aType)
				}
			}
		}
	}
	if act == nil {
		return
	}
	if _, hasIt := statsMp[act.ActionType]; !hasIt {
		statsMp[act.ActionType] = make(map[time.Time]bool)
	}
	statsMp[act.ActionType][now] = true
	mux.Unlock()
}

func (s *Scheduler) Loop() {
	s.schedulerStarted = true
	for {
		if !s.schedulerStarted { // shutdown requested
			break
		}
		for len(s.queue) == 0 { //hang here if empty
			<-s.restartLoop
		}
		utils.Logger.Info(fmt.Sprintf("<Scheduler> Scheduler queue length: %v", len(s.queue)))
		s.Lock()
		a0 := s.queue[0]
		utils.Logger.Info(fmt.Sprintf("<Scheduler> Action: %s", a0.ActionsID))
		now := time.Now()
		start := a0.GetNextStartTime(now)
		if start.Equal(now) || start.Before(now) {
			go a0.Execute(s.actSucessChan, s.actFailedChan)
			// if after execute the next start time is in the past then
			// do not add it to the queue
			a0.ResetStartTimeCache()
			now = time.Now().Add(time.Second)
			start = a0.GetNextStartTime(now)
			if start.Before(now) {
				s.queue = s.queue[1:]
			} else {
				s.queue = append(s.queue, a0)
				s.queue = s.queue[1:]
				sort.Sort(s.queue)
			}
			s.Unlock()
		} else {
			s.Unlock()
			d := a0.GetNextStartTime(now).Sub(now)
			utils.Logger.Info(fmt.Sprintf("<Scheduler> Time to next action (%s): %v", a0.ActionsID, d))
			s.timer = time.NewTimer(d)
			select {
			case <-s.timer.C:
				// timer has expired
				utils.Logger.Info(fmt.Sprintf("<Scheduler> Time for action on %s", a0.ActionsID))
			case <-s.restartLoop:
				// nothing to do, just continue the loop
			}
		}
	}
}

func (s *Scheduler) Reload() {
	s.loadActionPlans()
	s.restart()
}

func (s *Scheduler) loadActionPlans() {
	s.Lock()
	defer s.Unlock()
	// limit the number of concurrent tasks
	limit := make(chan bool, 10)
	// execute existing tasks
	for {
		task, err := s.storage.PopTask()
		if err != nil || task == nil {
			break
		}
		limit <- true
		go func() {
			utils.Logger.Info(fmt.Sprintf("<Scheduler> executing task %s on account %s", task.ActionsID, task.AccountID))
			task.Execute()
			<-limit
		}()
	}

	actionPlans, err := s.storage.GetAllActionPlans()
	if err != nil && err != utils.ErrNotFound {
		utils.Logger.Warning(fmt.Sprintf("<Scheduler> Cannot get action plans: %v", err))
	}
	utils.Logger.Info(fmt.Sprintf("<Scheduler> processing %d action plans", len(actionPlans)))
	// recreate the queue
	s.queue = engine.ActionTimingPriorityList{}
	for _, actionPlan := range actionPlans {
		if actionPlan == nil {
			continue
		}
		for _, at := range actionPlan.ActionTimings {
			if at.Timing == nil {
				utils.Logger.Warning(fmt.Sprintf("<Scheduler> Nil timing on action plan: %+v, discarding!", at))
				continue
			}
			if at.IsASAP() {
				continue
			}
			now := time.Now()
			if at.GetNextStartTime(now).Before(now) {
				// the task is obsolete, do not add it to the queue
				continue
			}
			at.SetAccountIDs(actionPlan.AccountIDs) // copy the accounts
			at.SetActionPlanID(actionPlan.Id)
			s.queue = append(s.queue, at)

		}
	}
	sort.Sort(s.queue)
	utils.Logger.Info(fmt.Sprintf("<Scheduler> queued %d action plans", len(s.queue)))
}

func (s *Scheduler) restart() {
	if s.schedulerStarted {
		s.restartLoop <- true
	}
	if s.timer != nil {
		s.timer.Stop()
	}
}

func (s *Scheduler) GetQueue() (queue engine.ActionTimingPriorityList) {
	s.RLock()
	utils.Clone(s.queue, &queue)
	defer s.RUnlock()
	return queue
}

func (s *Scheduler) Shutdown() {
	s.schedulerStarted = false // disable loop on next run
	s.restartLoop <- true      // cancel waiting tasks
	if s.timer != nil {
		s.timer.Stop()
	}
}
