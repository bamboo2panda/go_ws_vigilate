package handlers

import (
	"fmt"
	"log"
	"strconv"
	"time"
)

type job struct {
	HostServiceID int
}

func (j job) Run() {
	Repo.ScheduleCheck(j.HostServiceID)
}

func (repo *DBRepo) StartMonitoring() {
	if app.PreferenceMap["monitoring_live"] == "1" {

		// TODO trigger a message to broadcast to all clients that app is starting to monitor

		data := make(map[string]string)
		data["message"] = "Monitoring is starting..."

		err := app.WsClient.Trigger("publuc-channel", "app-starting", data)
		if err != nil {
			log.Println(err)
		}

		// get all of the services that we whant to monitor

		servicesToMonitor, err := repo.DB.GetServicesToMonitor()
		if err != nil {
			log.Println(err)
		}

		// range through the services
		for _, x := range servicesToMonitor {
			log.Println("*** Service to monitor on", x.HostName, "is", x.Service.ServiceName)

			// 	get the schedule unit and number

			var sch string

			if x.ScheduleUnit == "d" {
				sch = fmt.Sprintf("@every %d%s", x.ScheduleNumber*24, "h")
			} else {
				sch = fmt.Sprintf("@every %d%s", x.ScheduleNumber, x.ScheduleUnit)
			}
			// 	create a job
			var j job
			j.HostServiceID = x.ID
			scheuleID, err := app.Scheduler.AddJob(sch, j)
			if err != nil {
				log.Println(err)
			}

			//	save the id of the job so we can start/stop it
			app.MonitorMap[x.ID] = scheuleID

			// 	broadcast over websockets the fact that the service is scheduled
			payload := make(map[string]string)
			payload["message"] = "scheduling"
			data["host_service_id"] = strconv.Itoa(x.ID)
			yearone := time.Date(0001, 11, 17, 20, 34, 58, 65138737, time.UTC)
			if app.Scheduler.Entry(app.MonitorMap[x.HostID]).Next.After(yearone) {
				data["next_run"] = app.Scheduler.Entry(app.MonitorMap[x.HostID]).Next.Format("2006-01-02 3:04:05 PM")
			} else {
				data["next_run"] = "Pending..."
			}

			payload["host"] = x.HostName
			payload["service"] = x.Service.ServiceName
			if x.LastCheck.After(yearone) {
				payload["last_run"] = x.LastCheck.Format("2006-01-02 3:04:05 PM")
			} else {
				payload["last_run"] = "Pending..."
			}
			payload["schedule"] = fmt.Sprintf("@every %d%s", x.ScheduleNumber, x.ScheduleUnit)

			err = app.WsClient.Trigger("public-channel", "next-run-event", payload)
			if err != nil {
				log.Println(err)
			}

			err = app.WsClient.Trigger("public-channel", "schedule-changed-event", payload)
			if err != nil {
				log.Println(err)
			}

		}
	}
}
