//go:build linux

package main

import (
	"reflect"
	"testing"
)

func TestFindRootDsnoopIPC(t *testing.T) {
	semProc := `       key      semid perms      nsems   uid   gid  cuid  cgid      otime      ctime
      8427          0   600          1     0     0     0     0 1700000000 1700000000
      8427          1   600          1  1000   100  1000   100 1700000000 1700000000
      9999          2   600          1     0     0     0     0 1700000000 1700000000
`
	gotSem, err := findRootDsnoopIPC(semProc, "semid")
	if err != nil {
		t.Fatalf("sem parse error: %v", err)
	}
	if !reflect.DeepEqual(gotSem, []int{0}) {
		t.Fatalf("got semids %v, want [0]", gotSem)
	}

	shmProc := `       key      shmid perms       size  cpid  lpid nattch   uid   gid  cuid  cgid
      8427         14   600       4096   100   200      2     0     0     0     0
      8421         15   600       4096   101   201      1  1000   100  1000   100
`
	gotShm, err := findRootDsnoopIPC(shmProc, "shmid")
	if err != nil {
		t.Fatalf("shm parse error: %v", err)
	}
	if !reflect.DeepEqual(gotShm, []int{14}) {
		t.Fatalf("got shmids %v, want [14]", gotShm)
	}
}
