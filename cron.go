package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type cronJob struct {
	JobID   string
	Pid     int
	Command string
	Delete  bool `json:"-"`
}

func (job cronJob) ToString() string {
	marshal, _ := json.Marshal(job)
	return fmt.Sprintf("## BEGIN QOR JOB %v # %v\n%v\n## END QOR JOB\n", job.JobID, string(marshal), job.Command)
}

// Cron implemented a worker Queue based on cronjob
type Cron struct {
	Jobs     []*cronJob
	CronJobs []string
	mutex    sync.Mutex `sql:"-"`
}

// NewCronQueue initialize a Cron queue
func NewCronQueue() *Cron {
	return &Cron{}
}

func (cron *Cron) parseJobs() []*cronJob {
	cron.mutex.Lock()

	cron.Jobs = []*cronJob{}
	cron.CronJobs = []string{}
	if out, err := exec.Command("crontab", "-l").Output(); err == nil {
		var inQorJob bool
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(line, "## BEGIN QOR JOB") {
				inQorJob = true
				if idx := strings.Index(line, "{"); idx > 1 {
					var job cronJob
					if json.Unmarshal([]byte(line[idx-1:]), &job) == nil {
						cron.Jobs = append(cron.Jobs, &job)
					}
				}
			}

			if !inQorJob {
				cron.CronJobs = append(cron.CronJobs, line)
			}

			if strings.HasPrefix(line, "## END QOR JOB") {
				inQorJob = false
			}
		}
	}
	return cron.Jobs
}

func (cron *Cron) writeCronJob() error {
	defer cron.mutex.Unlock()

	cmd := exec.Command("crontab", "-")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	stdin, _ := cmd.StdinPipe()
	for _, cronJob := range cron.CronJobs {
		stdin.Write([]byte(cronJob + "\n"))
	}

	for _, job := range cron.Jobs {
		if !job.Delete {
			stdin.Write([]byte(job.ToString() + "\n"))
		}
	}
	stdin.Close()
	return cmd.Run()
}

// Add a job to cron queue
func (cron *Cron) Add(job QorJobInterface) (err error) {
	cron.parseJobs()
	defer cron.writeCronJob()

	var binaryFile string
	if binaryFile, err = filepath.Abs(os.Args[0]); err == nil {
		var jobs []*cronJob
		for _, cronJob := range cron.Jobs {
			if cronJob.JobID != job.GetJobID() {
				jobs = append(jobs, cronJob)
			}
		}

		if scheduler, ok := job.GetArgument().(Scheduler); ok && (scheduler.GetScheduleTime() != nil || scheduler.GetRepeatTime() != nil) {
			log.Printf("SCHEDULER: %+v", scheduler)

			log.Printf("ST: %+v", scheduler.GetScheduleTime())
			log.Printf("RT: %+v", scheduler.GetRepeatTime())

			scheduleTime := scheduler.GetScheduleTime()
			if scheduler.GetScheduleTime() != nil {
				st := scheduler.GetScheduleTime().In(time.Local)
				scheduleTime = &st
			}

			repeatTime := scheduler.GetRepeatTime()
			if scheduleTime != nil && scheduleTime.IsZero() && repeatTime == nil {
				execCommand(job, binaryFile, jobs, cron)
				cron.Jobs = jobs
				return
			}

			if (scheduleTime == nil || scheduleTime.IsZero()) && repeatTime != nil {
				newTime := time.Now()
				scheduleTime = &newTime
			}

			job.SetStatus(JobStatusScheduled)
			minute := strconv.Itoa(scheduleTime.Minute())
			hour := strconv.Itoa(scheduleTime.Hour())
			day := strconv.Itoa(scheduleTime.Day())
			month := fmt.Sprintf("%d", scheduleTime.Month())
			if repeatTime != nil {
				if *repeatTime >= time.Minute && *repeatTime < time.Hour {
					minute = fmt.Sprintf("*/%d", int(repeatTime.Minutes()))
					hour = "*"
					day = "*"
					month = "*"
				}
				if *repeatTime >= time.Hour && *repeatTime < (time.Hour*24) {
					hour = fmt.Sprintf("*/%d", int(repeatTime.Hours()))
					day = "*"
					month = "*"
				}
				if *repeatTime >= (time.Hour*24) && *repeatTime <= (time.Hour*24*30) {
					day = fmt.Sprintf("*/%d", int(repeatTime.Hours()/24))
					month = "*"
				}
			}
			currentPath, _ := os.Getwd()
			jobs = append(jobs, &cronJob{
				JobID:   job.GetJobID(),
				Command: fmt.Sprintf("%s %s %s %s * cd %v; %v --qor-job %v\n", minute, hour, day, month, currentPath, binaryFile, job.GetJobID()),
			})
		} else {
			execCommand(job, binaryFile, jobs, cron)
		}
		cron.Jobs = jobs
	}

	return
}

// Run a job from cron queue
func (cron *Cron) Run(qorJob QorJobInterface) error {
	job := qorJob.GetJob()

	if job.Handler != nil {
		go func() {
			sigint := make(chan os.Signal, 1)

			// interrupt signal sent from terminal
			signal.Notify(sigint, syscall.SIGINT)
			// sigterm signal sent from kubernetes
			signal.Notify(sigint, syscall.SIGTERM)

			i := <-sigint

			qorJob.SetProgressText(fmt.Sprintf("Worker killed by signal %s", i.String()))
			qorJob.SetStatus(JobStatusKilled)

			qorJob.StopReferesh()
			os.Exit(int(reflect.ValueOf(i).Int()))
		}()

		qorJob.StartReferesh()
		defer qorJob.StopReferesh()

		err := job.Handler(qorJob.GetSerializableArgument(qorJob), qorJob)
		if err == nil {
			cron.parseJobs()
			defer cron.writeCronJob()
			for _, cronJob := range cron.Jobs {
				if cronJob.JobID == qorJob.GetJobID() {
					cronJob.Delete = true
				}
				//add job if repeat time exists
				if scheduler, ok := qorJob.GetArgument().(Scheduler); ok && scheduler.GetRepeatTime() != nil {
					repeatTime := scheduler.GetRepeatTime()
					newScheduleTime := time.Now().Add(*repeatTime)
					qorJob.AddLog(fmt.Sprintf("--- repeating job ---, next run %s,  repeat %s", newScheduleTime, repeatTime))
					qorJob.SetStatus(JobStatusScheduled)
					cron.Add(qorJob)
				}
			}
		}
		return err
	}

	return errors.New("no handler found for job " + job.Name)
}

// Kill a job from cron queue
func (cron *Cron) Kill(job QorJobInterface) (err error) {
	cron.parseJobs()
	defer cron.writeCronJob()

	for _, cronJob := range cron.Jobs {
		if cronJob.JobID == job.GetJobID() {
			if process, err := os.FindProcess(cronJob.Pid); err == nil {
				if err = process.Kill(); err == nil {
					cronJob.Delete = true
					return nil
				}
			}
			return err
		}
	}
	return errors.New("failed to find job")
}

// Remove a job from cron queue
func (cron *Cron) Remove(job QorJobInterface) error {
	cron.parseJobs()
	defer cron.writeCronJob()

	for _, cronJob := range cron.Jobs {
		if cronJob.JobID == job.GetJobID() {
			if cronJob.Pid == 0 {
				cronJob.Delete = true
				return nil
			}
			return errors.New("failed to remove current job as it is running")
		}
	}
	return errors.New("failed to find job")
}

func execCommand(job QorJobInterface, binaryFile string, jobs []*cronJob, cron *Cron) error {
	var err error
	cmd := exec.Command(binaryFile, "--qor-job", job.GetJobID())
	if err := cmd.Start(); err == nil {
		jobs = append(jobs, &cronJob{JobID: job.GetJobID(), Pid: cmd.Process.Pid})
		cmd.Process.Release()
	}
	cron.Jobs = jobs
	return err
}
