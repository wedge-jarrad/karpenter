//go:build random_test_delay

/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package test

import (
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	"math/rand"
	"sync"
	"time"
)

// If the random_test_delay build flag is used, every Expect() call gets an additional random delay added to it.  This
// is intended to attempt to make tests more robust by eliminating tests that depend on timing.
func init() {
	gomega.Default = &gomegaWrapper{
		inner: gomega.Default,
		r:     rand.New(rand.NewSource(ginkgo.GinkgoRandomSeed())),
	}
}

type gomegaWrapper struct {
	inner gomega.Gomega
	mu    sync.Mutex
	r     *rand.Rand
}

func (g *gomegaWrapper) randomDelay() {
	g.mu.Lock()
	delay := time.Duration(g.r.Intn(5)) * time.Millisecond
	g.mu.Unlock()
	time.Sleep(delay)
}

func (g *gomegaWrapper) Ω(actual interface{}, extra ...interface{}) types.Assertion {
	g.randomDelay()
	return g.inner.Ω(actual, extra...)
}

func (g *gomegaWrapper) Expect(actual interface{}, extra ...interface{}) types.Assertion {
	g.randomDelay()
	return g.inner.Expect(actual, extra...)
}

func (g *gomegaWrapper) ExpectWithOffset(offset int, actual interface{}, extra ...interface{}) types.Assertion {
	g.randomDelay()
	return g.inner.ExpectWithOffset(offset, actual, extra...)
}

func (g *gomegaWrapper) Eventually(actual interface{}, intervals ...interface{}) types.AsyncAssertion {
	g.randomDelay()
	return g.inner.Eventually(actual, intervals...)
}

func (g *gomegaWrapper) EventuallyWithOffset(offset int, actual interface{}, intervals ...interface{}) types.AsyncAssertion {
	g.randomDelay()
	return g.inner.EventuallyWithOffset(offset, actual, intervals...)
}

func (g *gomegaWrapper) Consistently(actual interface{}, intervals ...interface{}) types.AsyncAssertion {
	g.randomDelay()
	return g.inner.Consistently(actual, intervals...)
}

func (g *gomegaWrapper) ConsistentlyWithOffset(offset int, actual interface{}, intervals ...interface{}) types.AsyncAssertion {
	g.randomDelay()
	return g.inner.ConsistentlyWithOffset(offset, actual, intervals...)
}

func (g *gomegaWrapper) SetDefaultEventuallyTimeout(duration time.Duration) {
	g.inner.SetDefaultEventuallyTimeout(duration)
}

func (g *gomegaWrapper) SetDefaultEventuallyPollingInterval(duration time.Duration) {
	g.inner.SetDefaultEventuallyPollingInterval(duration)
}

func (g *gomegaWrapper) SetDefaultConsistentlyDuration(duration time.Duration) {
	g.inner.SetDefaultConsistentlyDuration(duration)
}

func (g *gomegaWrapper) SetDefaultConsistentlyPollingInterval(duration time.Duration) {
	g.inner.SetDefaultConsistentlyPollingInterval(duration)
}

func (g *gomegaWrapper) Inner() gomega.Gomega {
	return g.inner
}
