package umi

import "math"

const (
	BigFilesPerDS = 3
	PerWANTarget  = 24
)

type NetEst struct {
	Conns  int     `json:"conns"`
	PerWAN float64 `json:"per_wan"`
	Enough bool    `json:"enough"`
}

type NetPlan struct {
	DSJobs   int     `json:"ds_jobs"`
	Jobs     int     `json:"jobs"`
	Parallel int     `json:"parallel"`
	Conns    int     `json:"conns"`
	PerWAN   float64 `json:"per_wan"`
	Enough   bool    `json:"enough"`
}

func Estimate(dsJobs, parallel, wan int) NetEst {
	if wan < 1 {
		wan = 1
	}
	conns := dsJobs * BigFilesPerDS * parallel
	perWAN := math.Round(float64(conns)/float64(wan)*10) / 10
	return NetEst{
		Conns:  conns,
		PerWAN: perWAN,
		Enough: conns >= PerWANTarget*wan,
	}
}

func Recommend(wan int) NetPlan {
	if wan < 1 {
		wan = 1
	} else if wan > 16 {
		wan = 16
	}
	dsJobs := 2 * wan
	if dsJobs > 8 {
		dsJobs = 8
	}
	parallel := 8
	if wan == 1 {
		parallel = 4
	} else if wan <= 3 {
		parallel = 6
	}
	jobs := 32
	if wan <= 2 {
		jobs = 16
	}
	est := Estimate(dsJobs, parallel, wan)
	return NetPlan{
		DSJobs:   dsJobs,
		Jobs:     jobs,
		Parallel: parallel,
		Conns:    est.Conns,
		PerWAN:   est.PerWAN,
		Enough:   est.Enough,
	}
}
