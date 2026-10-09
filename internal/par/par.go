// Package par runs indexed jobs on a bounded worker pool.
package par

import "sync"

// Do calls f(i) for i in [0, n) on up to workers goroutines.
func Do(n, workers int, f func(i int)) {
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				f(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}
