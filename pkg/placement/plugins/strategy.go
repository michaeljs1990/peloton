// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plugins

import (
	"github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc"
	"github.com/uber/peloton/pkg/placement/models"
)

// Strategy is a placment strategy that will do all the placement logic of
// assigning tasks to offers.
type Strategy interface {
	// GetTaskPlacements takes a list of assignments without any assigned offers and
	// will assign offers to the task in each assignment. This assignment is
	// returned as a map from task index to host index.
	// So for instance: map[int]int{2: 3} means that assignments[2] should
	// be assigned to hosts[3].
	// Tasks that could not be placed should map to an index of -1.
	GetTaskPlacements(assignments []*models.Assignment, hosts []*models.HostOffers) map[int]int

	// Filters will take a list of assignments and group them into groups that
	// should use the same host filter to acquire offers from the host manager.
	Filters(assignments []*models.Assignment) map[*hostsvc.HostFilter][]*models.Assignment

	// ConcurrencySafe returns true iff the strategy is concurrency safe. If
	// the strategy is concurrency safe then it is safe for multiple
	// go-routines to run the GetTaskPlacements method concurrently, else only one
	// go-routine is allowed to run the GetTaskPlacements method at a time.
	ConcurrencySafe() bool
}
