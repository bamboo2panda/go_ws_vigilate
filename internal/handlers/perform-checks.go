package handlers

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsawler/vigilate/internal/certificateutils"
	"github.com/tsawler/vigilate/internal/channeldata"
	"github.com/tsawler/vigilate/internal/helpers"
	"github.com/tsawler/vigilate/internal/models"
)

const (
	HTTP           = 1
	HTTPS          = 2
	SSLCertificate = 3
)

type jsonResp struct {
	OK            bool      `json:"ok"`
	Message       string    `json:"message"`
	ServiceID     int       `json:"service_id"`
	HostServiceID int       `json:"host_service_id"`
	HostID        int       `json:"host_id"`
	OldStatus     string    `json:"old_status"`
	NewStatus     string    `json:"new_status"`
	LastCheck     time.Time `json:"last_chesk"`
}

// ScheduleCheck perform of the scheduled check of the host service by id
func (repo *DBRepo) ScheduleCheck(hostServiceID int) {
	hs, err := repo.DB.GetHostServiceByID(hostServiceID)
	if err != nil {
		log.Println(err)
		return
	}

	h, err := repo.DB.GetHostByID(hs.HostID)
	if err != nil {
		log.Println(err)
		return
	}

	// test the service
	newStatus, msg := repo.testServiceForHost(h, hs)
	// if host service has changed - broadcast to all clients

	if newStatus != hs.Status {
		repo.updateHostServiceStatusCount(h, hs, newStatus, msg)
	}

}

func (repo *DBRepo) updateHostServiceStatusCount(h models.Host, hs models.HostService, newStatus, msg string) {

	// update host service record in db whith status (if chnged) and lact check record

	hs.Status = newStatus
	hs.LastMessage = msg
	hs.LastCheck = time.Now()
	err := repo.DB.UpdateHostService(hs)
	if err != nil {
		log.Println(err)
		return
	}

	pending, healthy, warning, problem, err := repo.DB.GetAllServicesStatusCounts()

	if err != nil {
		log.Println(err)
		return
	}

	data := make(map[string]string)
	data["healthy_count"] = strconv.Itoa(healthy)
	data["pending_count"] = strconv.Itoa(pending)
	data["problem_count"] = strconv.Itoa(problem)
	data["warning_count"] = strconv.Itoa(warning)
	repo.broadcastMessage("public-channel", "host-service-count-changed", data)

	log.Println("New status is", newStatus, "and msg is", msg)
}

func (repo *DBRepo) broadcastMessage(channel, messageType string, data map[string]string) {
	err := app.WsClient.Trigger(channel, messageType, data)
	if err != nil {
		log.Println(err)
	}
}

func (repo *DBRepo) TestCheck(w http.ResponseWriter, r *http.Request) {
	hostServiceID, _ := strconv.Atoi(chi.URLParam(r, "id"))
	oldStatus := chi.URLParam(r, "oldStatus")
	okay := true

	//get host sevice
	hs, err := repo.DB.GetHostServiceByID(hostServiceID)
	if err != nil {
		log.Println(err)
		okay = false
	}

	//get host?
	h, err := repo.DB.GetHostByID(hs.HostID)
	if err != nil {
		log.Println(err)
		okay = false
	}

	// test the service
	newStatus, msg := repo.testServiceForHost(h, hs)

	// broadcast service status changed event
	if newStatus != hs.Status {
		repo.pushStatusChangedEvent(h, hs, newStatus)
		//save event
		event := models.Event{
			EventType:     newStatus,
			HostServiceID: hs.ID,
			HostID:        h.ID,
			ServiceName:   hs.Service.ServiceName,
			HostName:      hs.HostName,
			Message:       msg,
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}
		err = repo.DB.InsertEvent(event)
		if err != nil {
			log.Println(err)
		}
	}

	// update the host service in the database whis status (if changed) and last check
	hs.Status = newStatus
	hs.LastCheck = time.Now()
	hs.UpdatedAt = time.Now()
	hs.LastMessage = msg

	err = repo.DB.UpdateHostService(hs)
	if err != nil {
		log.Println(err)
		okay = false
	}

	//create json to client
	var resp jsonResp
	if okay {
		resp = jsonResp{
			OK:            true,
			Message:       msg,
			ServiceID:     hs.ServiceID,
			HostServiceID: hs.ID,
			HostID:        h.ID,
			OldStatus:     oldStatus,
			NewStatus:     newStatus,
			LastCheck:     time.Now(),
		}
	} else {
		resp.OK = false
		resp.Message = "Something went wrong"
	}

	//send json to client

	out, _ := json.MarshalIndent(resp, "", "    ")
	w.Header().Set("Content_Type", "application/json")
	w.Write(out)
}

func (repo *DBRepo) testServiceForHost(h models.Host, hs models.HostService) (string, string) {
	var msg, newStatus string

	switch hs.ServiceID {
	case HTTP:
		msg, newStatus = testHTTPForHost(h.URL)
		break
	case HTTPS:
		msg, newStatus = testHTTPSForHost(h.URL)
		break
	case SSLCertificate:
		msg, newStatus = testSSLForHost(h.URL)
		break
	}

	// Broadcast to clients if appropriate
	if hs.Status != newStatus {
		repo.pushStatusChangedEvent(h, hs, newStatus)

		//save event
		event := models.Event{
			EventType:     newStatus,
			HostServiceID: hs.ID,
			HostID:        h.ID,
			ServiceName:   hs.Service.ServiceName,
			HostName:      hs.HostName,
			Message:       msg,
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}
		err := repo.DB.InsertEvent(event)
		if err != nil {
			log.Println(err)
		}

		// send email if appropriate
		if repo.App.PreferenceMap["notify_via_email"] == "1" {
			if hs.Status != "pending" {
				mm := channeldata.MailData{
					ToName:    repo.App.PreferenceMap["notify_name"],
					ToAddress: repo.App.PreferenceMap["notify_email"],
				}

				if newStatus == "healthy" {
					mm.Subject = fmt.Sprintf("HEALFY: service %s on %s", hs.Service.ServiceName, hs.HostName)
					mm.Content = template.HTML(fmt.Sprintf(`<p>Service %s on %s reported healthy status</p>
					<p><strong>Message recieved:</strong> %s </p>`, hs.Service.ServiceName, hs.HostName, msg))
				} else if newStatus == "problem" {
					mm.Subject = fmt.Sprintf("PROBLEM: service %s on %s", hs.Service.ServiceName, hs.HostName)
					mm.Content = template.HTML(fmt.Sprintf(`<p>Service %s on %s reported problem status</p>
					<p><strong>Message recieved:</strong> %s </p>`, hs.Service.ServiceName, hs.HostName, msg))
				} else if newStatus == "warning" {

				}

				helpers.SendEmail(mm)
			}
		}

		// TODO send sms if appropriate
	}

	repo.pushScheduleChangedEvent(hs, newStatus)

	return newStatus, msg
}

func (repo *DBRepo) pushStatusChangedEvent(h models.Host, hs models.HostService, newStatus string) {
	data := make(map[string]string)
	data["host_id"] = strconv.Itoa(hs.HostID)
	data["host_service_id"] = strconv.Itoa(hs.ID)
	data["host_name"] = h.HostName
	data["service_name"] = hs.Service.ServiceName
	data["icon"] = hs.Service.Icon
	data["status"] = newStatus
	data["message"] = fmt.Sprintf("%s on %s reports %s", hs.Service.ServiceName, h.HostName, newStatus)
	data["last_check"] = time.Now().Format("2006-01-02 3:04:05 PM")

	repo.broadcastMessage("public-channel", "host-service-status-changed", data)
}

func (repo *DBRepo) pushScheduleChangedEvent(hs models.HostService, newStatus string) {
	// broadcast schedule-change-event
	yearOne := time.Date(0001, 1, 1, 0, 0, 0, 1, time.UTC)
	data := make(map[string]string)

	data["host_service_id"] = strconv.Itoa(hs.ID)
	data["service_id"] = strconv.Itoa(hs.ServiceID)
	data["hsot_id"] = strconv.Itoa(hs.HostID)

	if app.Scheduler.Entry(repo.App.MonitorMap[hs.ID]).Next.After(yearOne) {
		data["next_run"] = repo.App.Scheduler.Entry(repo.App.MonitorMap[hs.ID]).Next.Format("2006-01-02 3:04:05 PM")
	} else {
		data["next_run"] = "Pending..."
	}

	data["last_run"] = time.Now().Format("2006-01-02 3:04:05 PM")
	data["host_name"] = hs.HostName
	data["service"] = hs.Service.ServiceName
	data["schedule"] = fmt.Sprintf("@every %d%s", hs.ScheduleNumber, hs.ScheduleUnit)
	data["status"] = newStatus
	data["icon"] = hs.Service.Icon

	repo.broadcastMessage("public-channel", "schedule-changed-event", data)
}

func testHTTPForHost(url string) (string, string) {
	url = strings.TrimSuffix(url, "/")
	url = strings.Replace(url, "https://", "http://", -1)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Sprintf("%s - %s", url, "error connectiong"), "problem"
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("%s - %s", url, resp.Status), "problem"
	}

	return fmt.Sprintf("%s - %s", url, resp.Status), "healthy"
}

func testHTTPSForHost(url string) (string, string) {
	url = strings.TrimSuffix(url, "/")
	url = strings.Replace(url, "http://", "https://", -1)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Sprintf("%s - %s", url, "error connectiong"), "problem"
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("%s - %s", url, resp.Status), "problem"
	}

	return fmt.Sprintf("%s - %s", url, resp.Status), "healthy"
}

func scanHost(hostname string, certDetailsChannel chan certificateutils.CertificateDetails, errorsChannel chan error) {

	res, err := certificateutils.GetCertificateDetails(hostname, 10)
	if err != nil {
		errorsChannel <- err
	} else {
		certDetailsChannel <- res
	}
}

func testSSLForHost(url string) (string, string) {
	if strings.HasPrefix(url, "https://") {
		url = strings.Replace(url, "https://", "", -1)
	}
	if strings.HasPrefix(url, "http://") {
		url = strings.Replace(url, "http://", "", -1)
	}
	// scanning ssl cert for expiry date
	var certDetailsChannel chan certificateutils.CertificateDetails
	var errorsChannel chan error
	certDetailsChannel = make(chan certificateutils.CertificateDetails, 1)
	errorsChannel = make(chan error, 1)

	var msg, newStatus string

	scanHost(url, certDetailsChannel, errorsChannel)

	for i, certDetailsInQueue := 0, len(certDetailsChannel); i < certDetailsInQueue; i++ {
		certDetails := <-certDetailsChannel
		certificateutils.CheckExpirationStatus(&certDetails, 30)

		if certDetails.ExpiringSoon {

			if certDetails.DaysUntilExpiration < 7 {
				msg = certDetails.Hostname + " expiring in " + strconv.Itoa(certDetails.DaysUntilExpiration) + " days"
				newStatus = "problem"
			} else {
				msg = certDetails.Hostname + " expiring in " + strconv.Itoa(certDetails.DaysUntilExpiration) + " days"
				newStatus = "warning"
			}

		} else {
			msg = certDetails.Hostname + " expiring in " + strconv.Itoa(certDetails.DaysUntilExpiration) + " days"
			newStatus = "healthy"
		}
	}

	return msg, newStatus

}

func (repo *DBRepo) addToMonitorMap(hs models.HostService) {
	if repo.App.PreferenceMap["monitoring_live"] == "1" {
		var j job
		j.HostServiceID = hs.ID
		scheduleID, err := repo.App.Scheduler.AddJob(fmt.Sprintf("@every %d%s", hs.ScheduleNumber, hs.ScheduleUnit), j)
		if err != nil {
			log.Println(err)
			return
		}
		repo.App.MonitorMap[hs.ID] = scheduleID
		data := make(map[string]string)
		data["message"] = "scheduling"
		data["host_service_id"] = strconv.Itoa(hs.ID)
		data["next_run"] = "Pending..."
		data["service"] = hs.Service.ServiceName
		data["host_name"] = hs.HostName
		data["last_run"] = hs.LastCheck.Format("2006-01-02 3:04:05 PM")
		data["schedule"] = fmt.Sprintf("@every %d%s", hs.ScheduleNumber, hs.ScheduleUnit)
		repo.broadcastMessage("public-channel", "schedule-changed-event", data)
	}
}

func (repo *DBRepo) removeFromMonitorMap(hs models.HostService) {
	if repo.App.PreferenceMap["monitoring_live"] == "1" {
		repo.App.Scheduler.Remove(repo.App.MonitorMap[hs.ID])
		data := make(map[string]string)
		data["host_service_id"] = strconv.Itoa(hs.ID)
		repo.broadcastMessage("public-channel", "schedule-item-removed-event", data)
	}
}
