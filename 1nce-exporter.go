package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type exporterConfiguration struct {
	credentials struct {
		username string
		password string
	}
	otlpEndpoint string
	otlpUsername string
	otlpPassword string
}

type onceExporter struct {
	configuration exporterConfiguration
	auth          struct {
		token   string
		expires time.Time
	}
	gauges struct {
		imeiLocked              metric.Float64Gauge
		dataVolumeRemaining     metric.Float64Gauge
		dataVolumeTotal         metric.Float64Gauge
		info                    metric.Float64Gauge
		lastContact             metric.Float64Gauge
		lastGprs                metric.Float64Gauge
		sessionStatus           metric.Float64Gauge
		activationStatus        metric.Float64Gauge
		positionLatitude        metric.Float64Gauge
		positionLongitude       metric.Float64Gauge
		positionResolvedSeconds metric.Float64Gauge
	}
}

var exporterState = onceExporter{}

func mustEnv(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		panic(fmt.Sprintf("required environment variable %s is not set", key))
	}
	return v
}

func parseUnixSeconds(s string) float64 {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.Unix())
		}
	}
	return 0
}

func load_configuration() {
	exporterState.configuration.credentials.username = mustEnv("ONCE_USERNAME")
	exporterState.configuration.credentials.password = mustEnv("ONCE_PASSWORD")
	exporterState.configuration.otlpEndpoint = mustEnv("OTLP_ENDPOINT")
	exporterState.configuration.otlpUsername = mustEnv("OTLP_USERNAME")
	exporterState.configuration.otlpPassword = mustEnv("OTLP_PASSWORD")
}

func onceAPIFetch(method string, reqURL string, headers map[string]string, payload io.Reader) []byte {
	req, err := http.NewRequest(method, reqURL, payload)
	if err != nil {
		log.Printf("failed to build request %s %s: %v", method, reqURL, err)
		return nil
	}

	for key, value := range headers {
		req.Header.Add(key, value)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("request failed %s %s: %v", method, reqURL, err)
		return nil
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		log.Printf("HTTP %s: %s %s", res.Status, method, reqURL)
		return nil
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		log.Printf("failed to read response from %s %s: %v", method, reqURL, err)
		return nil
	}
	return body
}

func fetchSimCards(ctx context.Context) {
	type OnceApiManagementSims []struct {
		Iccid          string `json:"iccid"`
		Imsi           string `json:"imsi"`
		Msisdn         string `json:"msisdn"`
		Imei           string `json:"imei,omitempty"`
		ImeiLock       bool   `json:"imei_lock"`
		Status         string `json:"status"`
		ActivationDate string `json:"activation_date"`
		IPAddress      string `json:"ip_address"`
		CurrentQuota   int    `json:"current_quota"`
		QuotaStatus    struct {
			ID          int    `json:"id"`
			Description string `json:"description"`
		} `json:"quota_status"`
		CurrentQuotaSMS int `json:"current_quota_SMS"`
		QuotaStatusSMS  struct {
			ID          int    `json:"id"`
			Description string `json:"description"`
		} `json:"quota_status_SMS"`
		Label string `json:"label,omitempty"`
	}

	headers := map[string]string{
		"accept":        "application/json",
		"authorization": fmt.Sprintf("Bearer %s", exporterState.auth.token)}
	body := onceAPIFetch("GET", "https://api.1nce.com/management-api/v1/sims?pageSize=100", headers, nil)

	var apiResponse OnceApiManagementSims
	err := json.Unmarshal(body, &apiResponse)
	if err != nil {
		log.Printf("JSON decode error: %v", err)
	}

	for _, simcard := range apiResponse {
		updateSimCardStatus(ctx, simcard.Iccid, simcard.Label, simcard.Status)
		updateSimCardDataQuota(ctx, simcard.Iccid)
		updateSimCardPosition(ctx, simcard.Iccid)

		imeiLockVal := 0.0
		if simcard.ImeiLock {
			imeiLockVal = 1.0
		}
		exporterState.gauges.imeiLocked.Record(ctx, imeiLockVal,
			metric.WithAttributes(
				attribute.String("iccid", simcard.Iccid),
				attribute.String("imei", simcard.Imei),
			))
	}
}

func activationStatusValue(status string) float64 {
	if status == "Enabled" {
		return 1
	}
	return 0
}

func sessionStatusValue(status string) float64 {
	switch status {
	case "ONLINE":
		return 2
	case "ATTACHED":
		return 1
	default:
		return 0
	}
}

func updateSimCardStatus(ctx context.Context, iccid, label, activationStatus string) {
	type OnceApiManagementSimCardStatus struct {
		Status   string `json:"status"`
		Location struct {
			Iccid           string `json:"iccid"`
			Imsi            string `json:"imsi"`
			LastUpdated     string `json:"last_updated"`
			LastUpdatedGprs string `json:"last_updated_gprs"`
			SgsnNumber      string `json:"sgsn_number"`
			VlrNumber       string `json:"vlr_number"`
			VlrNumberNp     string `json:"vlr_number_np"`
			MscNumberNp     string `json:"msc_number_np"`
			SgsnNumberNp    string `json:"sgsn_number_np"`
			OperatorID      string `json:"operator_id"`
			Msc             string `json:"msc"`
			Operator        struct {
				ID      int    `json:"id"`
				Name    string `json:"name"`
				Country struct {
					ID      int    `json:"id"`
					Name    string `json:"name"`
					IsoCode string `json:"iso_code"`
				} `json:"country"`
			} `json:"operator"`
			Country struct {
				CountryID   string `json:"country_id"`
				Name        string `json:"name"`
				CountryCode string `json:"country_code"`
				Mcc         string `json:"mcc"`
				IsoCode     string `json:"iso_code"`
				Latitude    string `json:"latitude"`
				Longitude   string `json:"longitude"`
			} `json:"country"`
			SgsnIPAddress string `json:"sgsn_ip_address"`
		} `json:"location"`
		PdpContext struct {
			PdpContextID              string `json:"pdp_context_id"`
			EndpointID                string `json:"endpoint_id"`
			TariffProfileID           string `json:"tariff_profile_id"`
			TariffID                  string `json:"tariff_id"`
			RatezoneID                string `json:"ratezone_id"`
			OrganisationID            string `json:"organisation_id"`
			ImsiID                    string `json:"imsi_id"`
			Imsi                      string `json:"imsi"`
			SimID                     string `json:"sim_id"`
			TeidDataPlane             string `json:"teid_data_plane"`
			TeidControlPlane          string `json:"teid_control_plane"`
			GtpVersion                string `json:"gtp_version"`
			Nsapi                     string `json:"nsapi"`
			SgsnControlPlaneIPAddress string `json:"sgsn_control_plane_ip_address"`
			SgsnDataPlaneIPAddress    string `json:"sgsn_data_plane_ip_address"`
			GgsnControlPlaneIPAddress string `json:"ggsn_control_plane_ip_address"`
			GgsnDataPlaneIPAddress    string `json:"ggsn_data_plane_ip_address"`
			Created                   string `json:"created"`
			Mcc                       string `json:"mcc"`
			Mnc                       string `json:"mnc"`
			OperatorID                string `json:"operator_id"`
			Lac                       string `json:"lac"`
			Ci                        string `json:"ci"`
			Sac                       string `json:"sac"`
			Rac                       string `json:"rac"`
			UeIPAddress               string `json:"ue_ip_address"`
			Imeisv                    string `json:"imeisv"`
			RatType                   struct {
				RatTypeID   string `json:"rat_type_id"`
				Description string `json:"description"`
			} `json:"rat_type"`
			Duration string `json:"duration"`
		} `json:"pdp_context"`
		Services []string `json:"services"`
	}

	headers := map[string]string{
		"accept":        "application/json",
		"authorization": fmt.Sprintf("Bearer %s", exporterState.auth.token)}
	body := onceAPIFetch("GET", fmt.Sprintf("https://api.1nce.com/management-api/v1/sims/%s/status", iccid), headers, nil)

	var apiResponse OnceApiManagementSimCardStatus
	err := json.Unmarshal(body, &apiResponse)
	if err != nil {
		log.Printf("JSON decode error: %v", err)
	}

	loc := apiResponse.Location
	log.Printf("[%s] status=%s operator=%s ip=%s last_contact=%s last_gprs=%s",
		iccid, apiResponse.Status,
		loc.Operator.Name,
		apiResponse.PdpContext.UeIPAddress,
		loc.LastUpdated, loc.LastUpdatedGprs)

	iccidAttr := metric.WithAttributes(attribute.String("iccid", iccid))

	exporterState.gauges.info.Record(ctx, 1.0,
		metric.WithAttributes(
			attribute.String("iccid", iccid),
			attribute.String("label", label),
			attribute.String("activation_status", activationStatus),
			attribute.String("operator", loc.Operator.Name),
			attribute.String("ip", apiResponse.PdpContext.UeIPAddress),
		))
	exporterState.gauges.sessionStatus.Record(ctx, sessionStatusValue(apiResponse.Status), iccidAttr)
	exporterState.gauges.activationStatus.Record(ctx, activationStatusValue(activationStatus), iccidAttr)
	exporterState.gauges.lastContact.Record(ctx, parseUnixSeconds(loc.LastUpdated), iccidAttr)
	exporterState.gauges.lastGprs.Record(ctx, parseUnixSeconds(loc.LastUpdatedGprs), iccidAttr)
}

func updateSimCardPosition(ctx context.Context, iccid string) {
	type PositionEntry struct {
		SampleTime string     `json:"sampleTime"`
		Coordinate [2]float64 `json:"coordinate"` // [longitude, latitude] — GeoJSON order
		Source     string     `json:"source"`
	}
	type PositionResponse struct {
		Coordinates []PositionEntry `json:"coordinates"`
	}

	headers := map[string]string{
		"accept":        "application/json",
		"authorization": fmt.Sprintf("Bearer %s", exporterState.auth.token)}
	body := onceAPIFetch("GET", fmt.Sprintf("https://api.1nce.com/management-api/v1/locate/devices/%s/positions", iccid), headers, nil)
	if body == nil {
		return
	}

	var resp PositionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		log.Printf("[%s] position decode error: %v — raw: %s", iccid, err, body)
		return
	}
	if len(resp.Coordinates) == 0 {
		return
	}

	pos := resp.Coordinates[0]
	for _, c := range resp.Coordinates[1:] {
		if parseUnixSeconds(c.SampleTime) > parseUnixSeconds(pos.SampleTime) {
			pos = c
		}
	}
	lon, lat := pos.Coordinate[0], pos.Coordinate[1]

	attrs := metric.WithAttributes(attribute.String("iccid", iccid))
	exporterState.gauges.positionLatitude.Record(ctx, lat, attrs)
	exporterState.gauges.positionLongitude.Record(ctx, lon, attrs)
	if ts := parseUnixSeconds(pos.SampleTime); ts > 0 {
		exporterState.gauges.positionResolvedSeconds.Record(ctx, ts, attrs)
	}
	log.Printf("[%s] position: lat=%.5f lon=%.5f source=%s sampled=%s", iccid, lat, lon, pos.Source, pos.SampleTime)
}

func updateSimCardDataQuota(ctx context.Context, iccid string) {
	type SimCardDataQuote struct {
		Volume               float64 `json:"volume"`
		TotalVolume          int     `json:"total_volume"`
		ExpiryDate           string  `json:"expiry_date"`
		PeakThroughput       int     `json:"peak_throughput"`
		LastVolumeAdded      int     `json:"last_volume_added"`
		LastStatusChangeDate string  `json:"last_status_change_date"`
		ThresholdPercentage  int     `json:"threshold_percentage"`
	}

	headers := map[string]string{
		"accept":        "application/json",
		"authorization": fmt.Sprintf("Bearer %s", exporterState.auth.token)}
	body := onceAPIFetch("GET", fmt.Sprintf("https://api.1nce.com/management-api/v1/sims/%s/quota/data", iccid), headers, nil)

	var apiResponse SimCardDataQuote
	err := json.Unmarshal(body, &apiResponse)
	if err != nil {
		log.Printf("JSON decode error: %v", err)
	}

	log.Printf("[%s] quota expires: %s  last status change: %s", iccid, apiResponse.ExpiryDate, apiResponse.LastStatusChangeDate)

	attrs := metric.WithAttributes(attribute.String("iccid", iccid))
	exporterState.gauges.dataVolumeTotal.Record(ctx, float64(apiResponse.TotalVolume), attrs)
	exporterState.gauges.dataVolumeRemaining.Record(ctx, apiResponse.Volume, attrs)
}

func requestBearer() {
	authorization := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(
		"%s:%s",
		exporterState.configuration.credentials.username,
		exporterState.configuration.credentials.password)))
	headers := map[string]string{
		"accept":        "application/json",
		"content-type":  "application/json",
		"authorization": fmt.Sprintf("Basic %s", authorization)}
	payload := strings.NewReader("{\"grant_type\":\"client_credentials\"}")
	body := onceAPIFetch("POST", "https://api.1nce.com/management-api/oauth/token", headers, payload)

	type onceApiOauthToken struct {
		StatusCode  int    `json:"status_code"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		UserId      string `json:"userId"`
		Scope       string `json:"scope"`
	}

	var apiResponse onceApiOauthToken

	err := json.Unmarshal(body, &apiResponse)
	if err != nil {
		log.Printf("JSON decode error: %v", err)
	}

	if apiResponse.AccessToken == "" {
		log.Printf("1nce bearer token request failed — check ONCE_USERNAME and ONCE_PASSWORD")
		return
	}
	exporterState.auth.token = apiResponse.AccessToken
	exporterState.auth.expires = time.Now().Add(time.Second * time.Duration(apiResponse.ExpiresIn-10))
	log.Printf("bearer token obtained, expires in %ds", apiResponse.ExpiresIn)
}

func checkAndRenewBearer() {
	if time.Now().After(exporterState.auth.expires) {
		requestBearer()
	}
}

func main() {
	load_configuration()
	checkAndRenewBearer()

	ctx := context.Background()

	otlpAuth := base64.StdEncoding.EncodeToString([]byte(
		exporterState.configuration.otlpUsername + ":" + exporterState.configuration.otlpPassword))

	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(exporterState.configuration.otlpEndpoint),
		otlpmetrichttp.WithHeaders(map[string]string{
			"Authorization": "Basic " + otlpAuth,
		}),
	)
	if err != nil {
		panic(fmt.Sprintf("cannot create OTLP exporter: %s", err))
	}

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer provider.Shutdown(ctx)

	meter := provider.Meter("1nce-exporter")

	exporterState.gauges.imeiLocked, err = meter.Float64Gauge("once_sim_card_imei_locked")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.dataVolumeRemaining, err = meter.Float64Gauge("once_sim_card_data_volume_remaining")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.dataVolumeTotal, err = meter.Float64Gauge("once_sim_card_data_volume_total")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.info, err = meter.Float64Gauge("once_sim_card_info")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.lastContact, err = meter.Float64Gauge("once_sim_card_last_contact_seconds")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.lastGprs, err = meter.Float64Gauge("once_sim_card_last_gprs_seconds")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.sessionStatus, err = meter.Float64Gauge("once_sim_card_session_status")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.activationStatus, err = meter.Float64Gauge("once_sim_card_activation_status")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.positionLatitude, err = meter.Float64Gauge("once_sim_card_position_latitude")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.positionLongitude, err = meter.Float64Gauge("once_sim_card_position_longitude")
	if err != nil {
		panic(err)
	}
	exporterState.gauges.positionResolvedSeconds, err = meter.Float64Gauge("once_sim_card_position_resolved_seconds")
	if err != nil {
		panic(err)
	}

	for {
		log.Printf("fetching SIM card metrics")
		checkAndRenewBearer()
		fetchSimCards(ctx)

		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			log.Printf("metrics collect error: %v", err)
		} else if err := exporter.Export(ctx, &rm); err != nil {
			if strings.Contains(err.Error(), "401") {
				log.Printf("OTLP authentication failed (check OTLP_USERNAME and OTLP_PASSWORD): %v", err)
			} else {
				log.Printf("OTLP push error: %v", err)
			}
		} else {
			log.Printf("metrics pushed successfully to %s", exporterState.configuration.otlpEndpoint)
		}

		time.Sleep(120 * time.Second)
	}
}
