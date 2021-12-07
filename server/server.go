/*
Copyright © 2020, 2021  Red Hat, Inc.

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

// Package server contains implementation of REST API server (HTTPServer) for
// the Insights results smart proxy service. In current version, the following
//
// Please note that API_PREFIX is part of server configuration (see
// Configuration). Also please note that JSON format is used to transfer data
// between server and clients.
//
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	// we just have to import this package in order to expose pprof
	// interface in debug mode
	// disable "G108 (CWE-): Profiling endpoint is automatically exposed on /debug/pprof"
	// #nosec G108
	_ "net/http/pprof"
	"path/filepath"

	"github.com/RedHatInsights/insights-content-service/groups"
	httputils "github.com/RedHatInsights/insights-operator-utils/http"
	"github.com/RedHatInsights/insights-operator-utils/responses"
	ira_server "github.com/RedHatInsights/insights-results-aggregator/server"
	ctypes "github.com/RedHatInsights/insights-results-types"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/rs/zerolog/log"

	"github.com/RedHatInsights/insights-results-smart-proxy/amsclient"
	"github.com/RedHatInsights/insights-results-smart-proxy/content"
	"github.com/RedHatInsights/insights-results-smart-proxy/services"

	"github.com/RedHatInsights/insights-results-smart-proxy/types"
)

const (
	// contentTypeHeader represents Content-Type header name
	contentTypeHeader = "Content-Type"

	// JSONContentType represents the application/json content type
	JSONContentType = "application/json; charset=utf-8"

	// orgIDTag represent the tags for print orgID in the logs
	orgIDTag = "orgID"

	// userIDTag represent the tags for print user ID (account number) in the logs
	userIDTag = "userID"
)

// HTTPServer is an implementation of Server interface
type HTTPServer struct {
	Config            Configuration
	InfoParams        map[string]string
	ServicesConfig    services.Configuration
	amsClient         amsclient.AMSClient
	GroupsChannel     chan []groups.Group
	ErrorFoundChannel chan bool
	ErrorChannel      chan error
	Serv              *http.Server
}

// RequestModifier is a type of function which modifies request when proxying
type RequestModifier func(request *http.Request) (*http.Request, error)

// ResponseModifier is a type of function which modifies response when proxying
type ResponseModifier func(response *http.Response) (*http.Response, error)

// ProxyOptions alters behaviour of proxy server for each endpoint.
// For example, you can set custom request and response modifiers
type ProxyOptions struct {
	RequestModifiers  []RequestModifier
	ResponseModifiers []ResponseModifier
}

// New function constructs new implementation of Server interface.
func New(config Configuration,
	servicesConfig services.Configuration,
	amsClient amsclient.AMSClient,
	groupsChannel chan []groups.Group,
	errorFoundChannel chan bool,
	errorChannel chan error,
) *HTTPServer {

	return &HTTPServer{
		Config:            config,
		InfoParams:        make(map[string]string),
		ServicesConfig:    servicesConfig,
		amsClient:         amsClient,
		GroupsChannel:     groupsChannel,
		ErrorFoundChannel: errorFoundChannel,
		ErrorChannel:      errorChannel,
	}
}

// mainEndpoint method handles requests to the main endpoint.
func (server *HTTPServer) mainEndpoint(writer http.ResponseWriter, _ *http.Request) {
	err := responses.SendOK(writer, responses.BuildOkResponse())
	if err != nil {
		log.Error().Err(err).Msg(responseDataError)
	}
}

// Initialize method performs the server initialization, including
// registration of all handlers.
func (server *HTTPServer) Initialize() http.Handler {
	log.Info().Msgf("Initializing HTTP server at '%s'", server.Config.Address)

	router := mux.NewRouter().StrictSlash(true)
	router.Use(httputils.LogRequest)

	apiPrefix := server.Config.APIv1Prefix

	metricsURL := apiPrefix + MetricsEndpoint
	openAPIv1URL := apiPrefix + filepath.Base(server.Config.APIv1SpecFile)
	openAPIv2URL := server.Config.APIv2Prefix + filepath.Base(server.Config.APIv2SpecFile)

	// enable authentication, but only if it is setup in configuration
	if server.Config.Auth {
		// we have to enable authentication for all endpoints,
		// including endpoints for Prometheus metrics and OpenAPI
		// specification, because there is not single prefix of other
		// REST API calls. The special endpoints needs to be handled in
		// middleware which is not optimal
		noAuthURLs := []string{
			metricsURL,
			openAPIv1URL,
			openAPIv2URL,
			metricsURL + "?",   // to be able to test using Frisby
			openAPIv1URL + "?", // to be able to test using Frisby
			openAPIv2URL + "?", // to be able to test using Frisby
		}
		router.Use(func(next http.Handler) http.Handler { return server.Authentication(next, noAuthURLs) })
	}

	if server.Config.EnableCORS {
		headersOK := handlers.AllowedHeaders([]string{
			"Content-Type",
			"Content-Length",
			"Accept-Encoding",
			"X-CSRF-Token",
			"Authorization",
		})
		originsOK := handlers.AllowedOrigins([]string{"*"})
		methodsOK := handlers.AllowedMethods([]string{
			http.MethodPost,
			http.MethodGet,
			http.MethodOptions,
			http.MethodPut,
			http.MethodDelete,
		})
		credsOK := handlers.AllowCredentials()
		corsMiddleware := handlers.CORS(originsOK, headersOK, methodsOK, credsOK)
		router.Use(corsMiddleware)
	}

	server.addEndpointsToRouter(router)

	return router
}

func (server *HTTPServer) addEndpointsToRouter(router *mux.Router) {
	// It is possible to use special REST API endpoints in debug mode
	if server.Config.Debug {
		server.adddbgEndpointsToRouter(router)
	}
	server.addV1EndpointsToRouter(router)
	server.addV2EndpointsToRouter(router)
}

// Start method starts HTTP or HTTPS server.
func (server *HTTPServer) Start() error {
	address := server.Config.Address
	log.Info().Msgf("Starting HTTP server at '%s'", address)
	router := server.Initialize()
	server.Serv = &http.Server{Addr: address, Handler: router}
	var err error

	if server.Config.UseHTTPS {
		err = server.Serv.ListenAndServeTLS("server.crt", "server.key")
	} else {
		err = server.Serv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Error().Err(err).Msg("Unable to start HTTP/S server")
		return err
	}

	return nil
}

// Stop method stops server's execution.
func (server *HTTPServer) Stop(ctx context.Context) error {
	return server.Serv.Shutdown(ctx)
}

// modifyRequest function modifies HTTP request during proxying it to another
// service.
// TODO: move to utils?
func modifyRequest(requestModifiers []RequestModifier, request *http.Request) (*http.Request, error) {
	for _, modifier := range requestModifiers {
		var err error
		request, err = modifier(request)
		if err != nil {
			return nil, err
		}
	}

	return request, nil
}

// modifyResponse function modifies HTTP response returned by another service
// during proxying.
// TODO: move to utils?
func modifyResponse(responseModifiers []ResponseModifier, response *http.Response) (*http.Response, error) {
	for _, modifier := range responseModifiers {
		var err error
		response, err = modifier(response)
		if err != nil {
			return nil, err
		}
	}

	return response, nil
}

// proxyTo method constructs proxy function to proxy request to another
// service.
func (server HTTPServer) proxyTo(baseURL string, options *ProxyOptions) func(http.ResponseWriter, *http.Request) {
	return func(writer http.ResponseWriter, request *http.Request) {
		if options != nil {
			var err error
			request, err = modifyRequest(options.RequestModifiers, request)
			if err != nil {
				handleServerError(writer, err)
				return
			}
		}

		log.Info().Msg("Handling response as a proxy")

		endpointURL, err := server.composeEndpoint(baseURL, request.RequestURI)
		if err != nil {
			log.Error().Err(err).Msgf("Error during endpoint %s URL parsing", request.RequestURI)
			handleServerError(writer, err)
			return
		}

		client := http.Client{}
		req, err := http.NewRequest(request.Method, endpointURL.String(), request.Body)
		if err != nil {
			panic(err)
		}

		copyHeader(request.Header, req.Header)

		response, body, err := server.sendRequest(client, req, options, writer)
		if err != nil {
			server.evaluateProxyError(writer, err, baseURL)
			return
		}

		// Maybe this code should be on responses.SendRaw or something like that
		err = responses.Send(response.StatusCode, writer, body)
		if err != nil {
			log.Error().Err(err).Msgf("Error writing the response")
			handleServerError(writer, err)
			return
		}
	}
}

// evaluateProxyError handles detected error in proxyTo
// according to its type and the requested baseURL
func (server HTTPServer) evaluateProxyError(writer http.ResponseWriter, err error, baseURL string) {
	if _, ok := err.(*url.Error); ok {
		switch baseURL {
		case server.ServicesConfig.AggregatorBaseEndpoint:
			handleServerError(writer, &AggregatorServiceUnavailableError{})
		case server.ServicesConfig.ContentBaseEndpoint:
			handleServerError(writer, &ContentServiceUnavailableError{})
		default:
			handleServerError(writer, err)
		}
	} else {
		handleServerError(writer, err)
	}
}

func (server HTTPServer) sendRequest(
	client http.Client, req *http.Request, options *ProxyOptions, writer http.ResponseWriter,
) (*http.Response, []byte, error) {
	log.Debug().Msgf("Connecting to %s", req.URL.RequestURI())
	response, err := client.Do(req)
	if err != nil {
		log.Error().Err(err).Msgf("Error during retrieve of %s", req.URL.RequestURI())
		return nil, nil, err
	}

	if options != nil {
		var err error
		response, err = modifyResponse(options.ResponseModifiers, response)
		if err != nil {
			return nil, nil, err
		}
	}

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Error().Err(err).Msgf("Error while retrieving content from request to %s", req.RequestURI)
		return nil, nil, err
	}

	return response, body, nil
}

func (server HTTPServer) composeEndpoint(baseEndpoint, currentEndpoint string) (*url.URL, error) {
	endpoint := strings.TrimPrefix(currentEndpoint, server.Config.APIv1Prefix)
	return url.Parse(baseEndpoint + endpoint)
}

func copyHeader(srcHeaders, dstHeaders http.Header) {
	for headerKey, headerValues := range srcHeaders {
		for _, value := range headerValues {
			dstHeaders.Add(headerKey, value)
		}
	}
}

// readClusterIDsForOrgID reads the list of clusters for a given
// organization from aggregator
func (server HTTPServer) readClusterIDsForOrgID(orgID ctypes.OrgID) ([]ctypes.ClusterName, error) {
	if server.amsClient != nil {
		clusterInfoList, _, err := server.amsClient.GetClustersForOrganization(
			orgID,
			nil,
			[]string{amsclient.StatusDeprovisioned, amsclient.StatusArchived},
		)
		if err == nil {
			log.Info().Int(orgIDTag, int(orgID)).Msgf("Number of cluster IDs retrieved from the AMS API: %v", len(clusterInfoList))
			clusterNames := types.GetClusterNames(clusterInfoList)
			return clusterNames, err
		}

		log.Error().Err(err).Msg("Error accessing amsclient")
	}

	if !server.Config.UseOrgClustersFallback {
		err := fmt.Errorf("amsclient not initialized")
		log.Error().Err(err).Msg("")
		return nil, err
	}

	log.Info().Msg("amsclient not initialized. Using fallback mechanism")
	return server.getClusterIDsFromAggregator(orgID)
}

// readClustersForOrgID returns a list of cluster info types and a map of cluster display names
func (server HTTPServer) readClustersForOrgID(orgID ctypes.OrgID) (
	[]types.ClusterInfo,
	map[types.ClusterName]string,
	error,
) {
	if server.amsClient != nil {
		clusterInfoList, clusterNamesMap, err := server.amsClient.GetClustersForOrganization(
			orgID,
			nil,
			[]string{amsclient.StatusDeprovisioned, amsclient.StatusArchived},
		)
		if err == nil {
			log.Info().Int(orgIDTag, int(orgID)).Msgf("Number of clusters retrieved from the AMS API: %v", len(clusterInfoList))
			return clusterInfoList, clusterNamesMap, nil
		}

		log.Error().Err(err).Msg("Error accessing amsclient")
	}

	if !server.Config.UseOrgClustersFallback {
		err := fmt.Errorf("amsclient not initialized")
		log.Error().Err(err).Msg("")
		return nil, nil, err
	}

	log.Info().Msg("amsclient not initialized. Using fallback mechanism")
	clusterIDs, err := server.getClusterIDsFromAggregator(orgID)
	if err != nil {
		log.Error().Err(err).Msg("error retrieving clusters from aggregator")
		return nil, nil, err
	}

	// fill in empty display names
	clusterNamesMap := make(map[types.ClusterName]string)
	clusterInfo := make([]types.ClusterInfo, len(clusterIDs))

	for i := range clusterIDs {
		clusterInfo = append(clusterInfo, types.ClusterInfo{ID: clusterIDs[i]})
		clusterNamesMap[clusterIDs[i]] = ""
	}
	return clusterInfo, clusterNamesMap, nil
}

// readClusterIDsForOrgID reads the list of clusters for a given organization from aggregator
func (server HTTPServer) getClusterIDsFromAggregator(orgID ctypes.OrgID) ([]ctypes.ClusterName, error) {
	log.Info().Msg("retrieving cluster IDs from aggregator")

	aggregatorURL := httputils.MakeURLToEndpoint(
		server.ServicesConfig.AggregatorBaseEndpoint,
		ira_server.ClustersForOrganizationEndpoint,
		orgID,
	)

	// #nosec G107
	response, err := http.Get(aggregatorURL)
	if err != nil {
		log.Error().Err(err).Msgf("problem getting cluster list from aggregator")
		if _, ok := err.(*url.Error); ok {
			return nil, &AggregatorServiceUnavailableError{}
		}
		return nil, err
	}

	var recvMsg struct {
		Status   string               `json:"status"`
		Clusters []ctypes.ClusterName `json:"clusters"`
	}

	err = json.NewDecoder(response.Body).Decode(&recvMsg)
	return recvMsg.Clusters, err
}

// readAggregatorReportForClusterID reads report from aggregator,
// handles errors by sending corresponding message to the user.
// Returns report and bool value set to true if there was no errors
func (server HTTPServer) readAggregatorReportForClusterID(
	orgID ctypes.OrgID, clusterID ctypes.ClusterName, userID ctypes.UserID, writer http.ResponseWriter,
) (*ctypes.ReportResponse, bool) {
	aggregatorURL := httputils.MakeURLToEndpoint(
		server.ServicesConfig.AggregatorBaseEndpoint,
		ira_server.ReportEndpoint,
		orgID,
		clusterID,
		userID,
	)

	// #nosec G107
	aggregatorResp, err := http.Get(aggregatorURL)
	if err != nil {
		if _, ok := err.(*url.Error); ok {
			handleServerError(writer, &AggregatorServiceUnavailableError{})
		} else {
			handleServerError(writer, err)
		}
		return nil, false
	}

	var aggregatorResponse struct {
		Report *ctypes.ReportResponse `json:"report"`
		Status string                 `json:"status"`
	}

	responseBytes, err := ioutil.ReadAll(aggregatorResp.Body)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	if aggregatorResp.StatusCode != http.StatusOK {
		err := responses.Send(aggregatorResp.StatusCode, writer, responseBytes)
		if err != nil {
			log.Error().Err(err).Msg(responseDataError)
		}
		return nil, false
	}

	err = json.Unmarshal(responseBytes, &aggregatorResponse)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}
	logClusterInfos(orgID, clusterID, aggregatorResponse.Report.Report)

	return aggregatorResponse.Report, true
}

// readAggregatorReportMetainfoForClusterID reads report metainfo from Aggregator,
// handles errors by sending corresponding message to the user.
// Returns report and bool value set to true if there was no errors
func (server HTTPServer) readAggregatorReportMetainfoForClusterID(
	orgID ctypes.OrgID, clusterID ctypes.ClusterName, userID ctypes.UserID, writer http.ResponseWriter,
) (*ctypes.ReportResponseMetainfo, bool) {
	aggregatorURL := httputils.MakeURLToEndpoint(
		server.ServicesConfig.AggregatorBaseEndpoint,
		ira_server.ReportMetainfoEndpoint,
		orgID,
		clusterID,
		userID,
	)

	// #nosec G107
	aggregatorResp, err := http.Get(aggregatorURL)
	if err != nil {
		if _, ok := err.(*url.Error); ok {
			handleServerError(writer, &AggregatorServiceUnavailableError{})
		} else {
			handleServerError(writer, err)
		}
		return nil, false
	}

	var aggregatorResponse struct {
		Metainfo *ctypes.ReportResponseMetainfo `json:"metainfo"`
		Status   string                         `json:"status"`
	}

	responseBytes, err := ioutil.ReadAll(aggregatorResp.Body)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	if aggregatorResp.StatusCode != http.StatusOK {
		err := responses.Send(aggregatorResp.StatusCode, writer, responseBytes)
		if err != nil {
			log.Error().Err(err).Msg(responseDataError)
		}
		return nil, false
	}

	err = json.Unmarshal(responseBytes, &aggregatorResponse)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	return aggregatorResponse.Metainfo, true
}

func (server HTTPServer) readAggregatorReportForClusterList(
	orgID ctypes.OrgID, clusterList []string, writer http.ResponseWriter,
) (*ctypes.ClusterReports, bool) {
	clist := strings.Join(clusterList, ",")
	aggregatorURL := httputils.MakeURLToEndpoint(
		server.ServicesConfig.AggregatorBaseEndpoint,
		ira_server.ReportForListOfClustersEndpoint,
		orgID,
		clist)

	// #nosec G107
	aggregatorResp, err := http.Get(aggregatorURL)
	if err != nil {
		if _, ok := err.(*url.Error); ok {
			handleServerError(writer, &AggregatorServiceUnavailableError{})
		} else {
			handleServerError(writer, err)
		}
		return nil, false
	}

	var aggregatorResponse ctypes.ClusterReports

	responseBytes, err := ioutil.ReadAll(aggregatorResp.Body)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	if aggregatorResp.StatusCode != http.StatusOK {
		err := responses.Send(aggregatorResp.StatusCode, writer, responseBytes)
		if err != nil {
			log.Error().Err(err).Msg(responseDataError)
		}
		return nil, false
	}

	err = json.Unmarshal(responseBytes, &aggregatorResponse)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}
	logClustersReport(orgID, aggregatorResponse.Reports)

	return &aggregatorResponse, true
}

func (server HTTPServer) readAggregatorReportForClusterListFromBody(
	orgID ctypes.OrgID, request *http.Request, writer http.ResponseWriter,
) (*ctypes.ClusterReports, bool) {
	aggregatorURL := httputils.MakeURLToEndpoint(
		server.ServicesConfig.AggregatorBaseEndpoint,
		ira_server.ReportForListOfClustersPayloadEndpoint,
		orgID,
	)

	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}
	// #nosec G107
	aggregatorResp, err := http.Post(aggregatorURL, JSONContentType, bytes.NewBuffer(body))
	if err != nil {
		if _, ok := err.(*url.Error); ok {
			handleServerError(writer, &AggregatorServiceUnavailableError{})
		} else {
			handleServerError(writer, err)
		}
		return nil, false
	}

	if reportResponse, ok := handleReportsResponse(aggregatorResp, writer); ok {
		logClustersReport(orgID, reportResponse.Reports)
		return reportResponse, true
	}
	return nil, false
}

// handleReportsResponse analyses the aggregator's response and
// writes an appropriate response to the client, handling any
// possible error in the meantime
func handleReportsResponse(response *http.Response, writer http.ResponseWriter) (*ctypes.ClusterReports, bool) {
	var aggregatorResponse ctypes.ClusterReports

	responseBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	if response.StatusCode != http.StatusOK {
		err := responses.Send(response.StatusCode, writer, responseBytes)
		if err != nil {
			log.Error().Err(err).Msg(responseDataError)
		}
		return nil, false
	}

	err = json.Unmarshal(responseBytes, &aggregatorResponse)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	return &aggregatorResponse, true
}

// readAggregatorRuleForClusterID reads report from aggregator,
// handles errors by sending corresponding message to the user.
// Returns report and bool value set to true if there was no errors
func (server HTTPServer) readAggregatorRuleForClusterID(
	orgID ctypes.OrgID, clusterID ctypes.ClusterName, userID ctypes.UserID, ruleID ctypes.RuleID, errorKey ctypes.ErrorKey, writer http.ResponseWriter,
) (*ctypes.RuleOnReport, bool) {
	aggregatorURL := httputils.MakeURLToEndpoint(
		server.ServicesConfig.AggregatorBaseEndpoint,
		ira_server.RuleEndpoint,
		orgID,
		clusterID,
		userID,
		fmt.Sprintf("%v|%v", ruleID, errorKey),
	)

	// #nosec G107
	aggregatorResp, err := http.Get(aggregatorURL)
	if err != nil {
		if _, ok := err.(*url.Error); ok {
			handleServerError(writer, &AggregatorServiceUnavailableError{})
		} else {
			handleServerError(writer, err)
		}
		return nil, false
	}

	var aggregatorResponse struct {
		Report *ctypes.RuleOnReport `json:"report"`
		Status string               `json:"status"`
	}

	responseBytes, err := ioutil.ReadAll(aggregatorResp.Body)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	if aggregatorResp.StatusCode != http.StatusOK {
		err := responses.Send(aggregatorResp.StatusCode, writer, responseBytes)
		if err != nil {
			log.Error().Err(err).Msg(responseDataError)
		}
		return nil, false
	}

	err = json.Unmarshal(responseBytes, &aggregatorResponse)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}
	logClusterInfo(orgID, clusterID, aggregatorResponse.Report)

	return aggregatorResponse.Report, true
}

func (server HTTPServer) fetchAggregatorReport(
	writer http.ResponseWriter, request *http.Request,
) (aggregatorResponse *ctypes.ReportResponse, successful bool, clusterID ctypes.ClusterName) {
	clusterID, successful = httputils.ReadClusterName(writer, request)
	// Error message handled by function
	if !successful {
		return
	}

	authToken, err := server.GetAuthToken(request)
	if err != nil {
		handleServerError(writer, err)
		return
	}

	userID := authToken.AccountNumber
	orgID := authToken.Internal.OrgID

	aggregatorResponse, successful = server.readAggregatorReportForClusterID(orgID, clusterID, userID, writer)
	if !successful {
		return
	}
	return
}

// fetchAggregatorReportMetainfo method tries to fetch metainformation about
// report for selected cluster.
func (server HTTPServer) fetchAggregatorReportMetainfo(
	writer http.ResponseWriter, request *http.Request,
) (aggregatorResponse *ctypes.ReportResponseMetainfo, successful bool, clusterID ctypes.ClusterName) {
	clusterID, successful = httputils.ReadClusterName(writer, request)
	// Error message handled by function
	if !successful {
		return
	}

	authToken, err := server.GetAuthToken(request)
	if err != nil {
		handleServerError(writer, err)
		return
	}

	userID := authToken.AccountNumber
	orgID := authToken.Internal.OrgID

	aggregatorResponse, successful = server.readAggregatorReportMetainfoForClusterID(orgID, clusterID, userID, writer)
	if !successful {
		return
	}
	return
}

// fetchAggregatorReports method access the Insights Results Aggregator to read
// reports for given list of clusters. Then the response structure is
// constructed from data returned by Aggregator.
func (server HTTPServer) fetchAggregatorReports(
	writer http.ResponseWriter, request *http.Request,
) (*ctypes.ClusterReports, bool) {
	// cluster list is specified in path (part of URL)
	clusterList, successful := httputils.ReadClusterListFromPath(writer, request)
	// Error message handled by function
	if !successful {
		return nil, false
	}

	authToken, err := server.GetAuthToken(request)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	orgID := authToken.Internal.OrgID

	aggregatorResponse, successful := server.readAggregatorReportForClusterList(orgID, clusterList, writer)
	if !successful {
		return nil, false
	}
	return aggregatorResponse, true
}

// fetchAggregatorReportsUsingRequestBodyClusterList method access the Insights
// Results Aggregator to read reports for given list of clusters. Then the
// response structure is constructed from data returned by Aggregator.
func (server HTTPServer) fetchAggregatorReportsUsingRequestBodyClusterList(
	writer http.ResponseWriter, request *http.Request,
) (*ctypes.ClusterReports, bool) {
	// auth token from request headers
	authToken, err := server.GetAuthToken(request)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	orgID := authToken.Internal.OrgID

	aggregatorResponse, successful := server.readAggregatorReportForClusterListFromBody(orgID, request, writer)
	if !successful {
		return nil, false
	}
	return aggregatorResponse, true
}

func (server HTTPServer) reportEndpoint(writer http.ResponseWriter, request *http.Request) {
	aggregatorResponse, successful, clusterID := server.fetchAggregatorReport(writer, request)
	if !successful {
		return
	}

	includeDisabled, err := readGetDisabledParam(request)
	if err != nil {
		handleServerError(writer, err)
		return
	}

	osdFlag, err := readOSDEligible(request)
	if err != nil {
		log.Err(err).Msgf("Cluster ID: %v; Got error while parsing `%s` value", clusterID, OSDEligibleParam)
	}

	log.Info().Msgf("Cluster ID: %v; %s flag = %t", clusterID, GetDisabledParam, includeDisabled)
	log.Info().Msgf("Cluster ID: %v; %s flag = %t", clusterID, OSDEligibleParam, osdFlag)

	visibleRules, noContentRulesCnt, disabledRulesCnt, err := filterRulesInResponse(aggregatorResponse.Report, osdFlag, includeDisabled)

	if err != nil {
		if _, ok := err.(*content.RuleContentDirectoryTimeoutError); ok {
			handleServerError(writer, err)
			return
		}
	}

	totalRuleCnt := server.getRuleCount(visibleRules, noContentRulesCnt, disabledRulesCnt, clusterID)

	// Meta.Count is only used to perform checks for special cases
	report := types.SmartProxyReport{
		Meta: ctypes.ReportResponseMeta{
			LastCheckedAt: aggregatorResponse.Meta.LastCheckedAt,
			Count:         totalRuleCnt,
		},
		Data: visibleRules,
	}

	err = responses.SendOK(writer, responses.BuildOkResponseWithData("report", report))
	if err != nil {
		log.Error().Err(err).Msg(responseDataError)
	}
}

// readMetainfo method retrieves metainformations for report stored in
// Aggregator's database and return the retrieved info to requester via
// response payload. The payload has type types.ReportResponseMetainfo
func (server HTTPServer) reportMetainfoEndpoint(writer http.ResponseWriter, request *http.Request) {
	aggregatorResponse, successful, clusterID := server.fetchAggregatorReportMetainfo(writer, request)
	if !successful {
		return
	}

	log.Info().Msgf("Metainfo returned by aggregator for cluster %s: %v", clusterID, aggregatorResponse)

	err := responses.SendOK(writer, responses.BuildOkResponseWithData("metainfo", aggregatorResponse))
	if err != nil {
		log.Error().Err(err).Msg(responseDataError)
	}
}

// getRuleCount returns the number of visible rules without those that do not have content
func (server HTTPServer) getRuleCount(visibleRules []types.RuleWithContentResponse,
	noContentRulesCnt int,
	disabledRulesCnt int,
	clusterID ctypes.ClusterName,
) int {
	totalRuleCnt := len(visibleRules) + noContentRulesCnt

	// Edge case where rules are hitting, but we don't have content for any of them.
	// This case should appear as "No issues found" in customer-facing applications, because the only
	// thing we could show is rule module + error key, which have no informational value to customers.
	if len(visibleRules) == 0 && noContentRulesCnt > 0 && disabledRulesCnt == 0 {
		log.Error().Msgf("Cluster ID: %v; Rules are hitting, but we don't have content for any of them.", clusterID)
		totalRuleCnt = 0
	}
	return totalRuleCnt
}

// reportForListOfClustersEndpoint is a handler that returns reports for
// several clusters that all need to belong to one organization specified in
// request path. List of clusters is specified in request path as well which
// means that clients needs to deal with URL limit (around 2000 characters).
func (server HTTPServer) reportForListOfClustersEndpoint(writer http.ResponseWriter, request *http.Request) {
	// try to read results from Insights Results Aggregator service
	aggregatorResponse, successful := server.fetchAggregatorReports(writer, request)
	if !successful {
		return
	}

	// send the response back to client
	err := responses.Send(http.StatusOK, writer, aggregatorResponse)
	if err != nil {
		log.Error().Err(err).Msg(responseDataError)
	}
}

// reportForListOfClustersPayloadEndpoint is a handler that returns reports for
// several clusters that all need to belong to one organization specified in
// request path. List of clusters is specified in request body which means that
// clients can use as many cluster ID as the wont without any (real) limits.
func (server HTTPServer) reportForListOfClustersPayloadEndpoint(writer http.ResponseWriter, request *http.Request) {
	// try to read results from Insights Results Aggregator service
	aggregatorResponse, successful := server.fetchAggregatorReportsUsingRequestBodyClusterList(writer, request)
	if !successful {
		return
	}

	// send the response back to client
	err := responses.Send(http.StatusOK, writer, aggregatorResponse)
	if err != nil {
		log.Error().Err(err).Msg(responseDataError)
	}
}

func (server HTTPServer) fetchAggregatorReportRule(
	writer http.ResponseWriter, request *http.Request,
) (*ctypes.RuleOnReport, bool) {
	clusterID, successful := httputils.ReadClusterName(writer, request)
	// Error message handled by function
	if !successful {
		return nil, false
	}

	ruleID, errorKey, err := readRuleIDWithErrorKey(writer, request)
	if err != nil {
		return nil, false
	}

	authToken, err := server.GetAuthToken(request)
	if err != nil {
		handleServerError(writer, err)
		return nil, false
	}

	userID := authToken.AccountNumber
	orgID := authToken.Internal.OrgID

	aggregatorResponse, successful := server.readAggregatorRuleForClusterID(orgID, clusterID, userID, ruleID, errorKey, writer)
	if !successful {
		return nil, false
	}
	return aggregatorResponse, true
}

func (server HTTPServer) singleRuleEndpoint(writer http.ResponseWriter, request *http.Request) {
	var rule *types.RuleWithContentResponse
	var filtered bool
	var err error

	aggregatorResponse, successful := server.fetchAggregatorReportRule(writer, request)
	// Error message handled by function
	if !successful {
		return
	}

	osdFlag, err := readOSDEligible(request)
	if err != nil {
		log.Err(err).Msgf("Got error while parsing `%s` value", OSDEligibleParam)
	}
	rule, filtered, err = content.FetchRuleContent(*aggregatorResponse, osdFlag)

	if err != nil || filtered {
		handleFetchRuleContentError(writer, err, filtered)
		return
	}

	if rule.Internal {
		err = server.checkInternalRulePermissions(request)
		if err != nil {
			handleServerError(writer, err)
			return
		}
	}

	err = responses.SendOK(writer, responses.BuildOkResponseWithData("report", *rule))
	if err != nil {
		log.Error().Err(err).Msg(responseDataError)
	}
}

func handleFetchRuleContentError(writer http.ResponseWriter, err error, filtered bool) {
	if err != nil {
		if _, ok := err.(*content.RuleContentDirectoryTimeoutError); ok {
			log.Error().Err(err)
			handleServerError(writer, err)
			return
		}
	}
	err = responses.SendNotFound(writer, "Rule was not found")
	if err != nil {
		handleServerError(writer, err)
		return
	}
}

// checkInternalRulePermissions method checks if organizations for internal
// rules are enabled if so, retrieves the org_id from request/token and returns
// whether that ID is on the list of allowed organizations to access internal
// rules
func (server HTTPServer) checkInternalRulePermissions(request *http.Request) error {
	if !server.Config.EnableInternalRulesOrganizations || !server.Config.Auth {
		return nil
	}

	authToken, err := server.GetAuthToken(request)
	if err != nil {
		return err
	}

	requestOrgID := ctypes.OrgID(authToken.Internal.OrgID)

	log.Info().Msgf("Checking internal rule permissions for Organization ID: %v", requestOrgID)
	for _, allowedID := range server.Config.InternalRulesOrganizations {
		if requestOrgID == allowedID {
			log.Info().Msgf("Organization %v is allowed access to internal rules", requestOrgID)
			return nil
		}
	}

	// If the loop ends without returning nil, then an authentication error should be raised
	const message = "This organization is not allowed to access this recommendation"
	log.Error().Msg(message)
	return &AuthenticationError{errString: message}
}

func (server HTTPServer) newExtractUserIDFromTokenToURLRequestModifier(newEndpoint string) RequestModifier {
	return func(request *http.Request) (*http.Request, error) {
		identity, err := server.GetAuthToken(request)
		if err != nil {
			return nil, err
		}

		vars := mux.Vars(request)
		vars["user_id"] = string(identity.AccountNumber)

		newURL := httputils.MakeURLToEndpointMapString(server.Config.APIv1Prefix, newEndpoint, vars)
		request.URL, err = url.Parse(newURL)
		if err != nil {
			return nil, &ParamsParsingError{}
		}

		request.RequestURI = request.URL.RequestURI()

		return request, nil
	}
}

// getGroupsConfig retrieves the groups configuration from a channel to get the
// latest valid one
func (server HTTPServer) getGroupsConfig() (
	ruleGroups []groups.Group,
	err error,
) {
	var errorFound bool
	ruleGroups = []groups.Group{}

	select {
	case val, ok := <-server.ErrorFoundChannel:
		if !ok {
			log.Error().Msgf("errorFound channel is closed")
			return
		}
		errorFound = val
	default:
		fmt.Println("errorFound channel is empty")
		return
	}

	if errorFound {
		err = <-server.ErrorChannel
		if _, ok := err.(*content.RuleContentDirectoryTimeoutError); ok {
			log.Error().Err(err)
		}
		log.Error().Err(err).Msg("Error occurred during groups retrieval from content service")
		return nil, err
	}

	groupsConfig := <-server.GroupsChannel
	if groupsConfig == nil {
		err := errors.New("no groups retrieved")
		log.Error().Err(err).Msg("groups cannot be retrieved from content service. Check logs")
		return nil, err
	}

	return groupsConfig, nil
}

func (server HTTPServer) getOverviewPerCluster(
	clusterName ctypes.ClusterName,
	authToken *ctypes.Identity,
	writer http.ResponseWriter) (*types.ClusterOverview, error) {

	userID := authToken.AccountNumber
	orgID := authToken.Internal.OrgID
	aggregatorResponse, successful := server.readAggregatorReportForClusterID(orgID, clusterName, userID, writer)
	if !successful {
		log.Info().Msgf("Aggregator doesn't have reports for cluster ID %s", clusterName)
		return nil, nil
	}

	if aggregatorResponse.Meta.Count == 0 {
		log.Info().Msgf("Cluster report doesn't have any hits. Skipping from overview.")
		return nil, nil
	}

	totalRisks := make([]int, 0)
	tags := make([]string, 0)

	for _, rule := range aggregatorResponse.Report {
		ruleID := rule.Module
		errorKey := rule.ErrorKey
		ruleWithContent, err := content.GetRuleWithErrorKeyContent(ruleID, errorKey)
		if err != nil {
			if _, ok := err.(*content.RuleContentDirectoryTimeoutError); ok {
				log.Error().Err(err)
				return nil, err
			}
			log.Error().Err(err).Msgf("Unable to retrieve content for rule %v|%v", ruleID, errorKey)
			// this rule is not visible in OCM UI either, so we can continue calculating to be consistent
			continue
		}

		totalRisks = append(totalRisks, ruleWithContent.TotalRisk)

		tags = append(tags, ruleWithContent.Tags...)
	}

	return &types.ClusterOverview{
		TotalRisksHit: totalRisks,
		TagsHit:       tags,
	}, nil
}

// filterRulesInResponse returns an array of RuleWithContentResponse with only the rules that matches 3 criteria:
// - The rule has content from the content-service
// - The disabled filter is not match
// - The OSD elegible filter is not match
func filterRulesInResponse(aggregatorReport []ctypes.RuleOnReport, filterOSD, getDisabled bool) (
	okRules []types.RuleWithContentResponse,
	noContentRulesCnt int,
	disabledRulesCnt int,
	contentError error,
) {
	log.Debug().Bool(GetDisabledParam, getDisabled).Bool(OSDEligibleParam, filterOSD).Msg("Filtering rules in report")
	okRules = []types.RuleWithContentResponse{}
	disabledRulesCnt, noContentRulesCnt = 0, 0

	for _, aggregatorRule := range aggregatorReport {
		if aggregatorRule.Disabled && !getDisabled {
			disabledRulesCnt++
			continue
		}

		rule, filtered, err := content.FetchRuleContent(aggregatorRule, filterOSD)
		if err != nil {
			if !filtered {
				noContentRulesCnt++
			}
			if _, ok := err.(*content.RuleContentDirectoryTimeoutError); ok {
				log.Error().Err(err)
				contentError = err
				return
			}
			continue
		}

		if filtered {
			continue
		}

		okRules = append(okRules, *rule)
	}

	return
}
