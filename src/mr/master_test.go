package mr


import (
	"testing"
	"time"
)


func TestMaster(t *testing.T) {
	c := Coordinator{}

	// Your code here.


	c.server()

	cnt := 10

	for c.Done() == false {
		time.Sleep(time.Second)
		cnt--
		if cnt == 0 {
			break
		}
	}
}