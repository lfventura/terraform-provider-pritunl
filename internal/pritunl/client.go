package pritunl

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type Client interface {
	TestApiCall() error

	GetOrganizations() ([]Organization, error)
	GetOrganization(id string) (*Organization, error)
	CreateOrganization(name string) (*Organization, error)
	UpdateOrganization(id string, organization *Organization) error
	DeleteOrganization(name string) error

	GetUser(id string, orgId string) (*User, error)
	CreateUser(newUser User) (*User, error)
	UpdateUser(id string, user *User) error
	DeleteUser(id string, orgId string) error

	GetServers() ([]Server, error)
	GetServer(id string) (*Server, error)
	CreateServer(serverData map[string]interface{}) (*Server, error)
	UpdateServer(id string, server *Server) error
	DeleteServer(id string) error

	GetOrganizationsByServer(serverId string) ([]Organization, error)
	AttachOrganizationToServer(organizationId, serverId string) error
	DetachOrganizationFromServer(organizationId, serverId string) error

	GetRoutesByServer(serverId string) ([]Route, error)
	AddRouteToServer(serverId string, route Route) error
	AddRoutesToServer(serverId string, route []Route) error
	DeleteRouteFromServer(serverId string, route Route) error
	UpdateRouteOnServer(serverId string, route Route) error

	GetHosts() ([]Host, error)
	GetHost(id string) (*Host, error)
	UpdateHost(id string, host HostUpdate) (*Host, error)
	GetHostsByServer(serverId string) ([]Host, error)
	AttachHostToServer(hostId, serverId string) error
	DetachHostFromServer(hostId, serverId string) error

	StartServer(serverId string) error
	StopServer(serverId string) error

	PingWebServer(ctx context.Context) error
	GetSettings() (Settings, error)
	UpdateSettings(settings Settings) error

	GetSubscription() (Subscription, error)
	ActivateSubscription(license string) error
	DeleteSubscription() error

	GetAdministrators() ([]Administrator, error)
	GetAdministrator(id string) (Administrator, error)
	CreateAdministrator(administrator Administrator) (Administrator, error)
	UpdateAdministrator(id string, administrator Administrator) error
	DeleteAdministrator(id string) error
}

type client struct {
	httpClient *http.Client
	baseUrl    string
}

func (c client) TestApiCall() error {
	url := fmt.Sprintf("/state")
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("TestApiCall: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on the tests api call\ncode=%d\nbody=%s\n", resp.StatusCode, body)
	}

	// 401 - invalid credentials
	if resp.StatusCode == 401 {
		return fmt.Errorf("unauthorized: Invalid token or secret")
	}

	return nil
}

func (c client) GetOrganization(id string) (*Organization, error) {
	url := fmt.Sprintf("/organization/%s", id)
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetOrganization: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting the organization\nbody=%s", body)
	}

	var organization Organization

	err = json.Unmarshal(body, &organization)
	if err != nil {
		return nil, fmt.Errorf("GetOrganization: %s: %+v, id=%s, body=%s", err, organization, id, body)
	}

	return &organization, nil
}

func (c client) GetOrganizations() ([]Organization, error) {
	url := fmt.Sprintf("/organization")
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetOrganization: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting the organization\nbody=%s", body)
	}

	var organizations []Organization

	err = json.Unmarshal(body, &organizations)
	if err != nil {
		return nil, fmt.Errorf("GetOrganization: %s: %+v, body=%s", err, organizations, body)
	}

	return organizations, nil
}

func (c client) CreateOrganization(name string) (*Organization, error) {
	var jsonStr = []byte(`{"name": "` + name + `"}`)

	known, err := c.organizationIdsByName(name)
	if err != nil {
		return nil, fmt.Errorf("CreateOrganization: %s", err)
	}

	var created *Organization

	result, adopted, err := retryCreate(
		func() attemptResult {
			return c.post("/organization", jsonStr)
		},
		func() (bool, error) {
			live, err := c.GetOrganizations()
			if err != nil {
				return false, err
			}

			var ids []string
			byId := map[string]Organization{}
			for _, organization := range live {
				if organization.Name == name {
					ids = append(ids, organization.ID)
					byId[organization.ID] = organization
				}
			}

			id, found, err := pickCreated("organizations", name, newIds(known, ids))
			if err != nil || !found {
				return false, err
			}
			organization := byId[id]
			created = &organization

			return true, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("CreateOrganization: %s", err)
	}
	if adopted {
		return created, nil
	}
	if result.err != nil {
		return nil, fmt.Errorf("CreateOrganization: Error on HTTP request: %s", result.err)
	}
	if result.status != 200 {
		return nil, fmt.Errorf("Non-200 response on creating the organization\ncode=%d\nbody=%s", result.status, result.body)
	}

	var organization Organization
	err = json.Unmarshal(result.body, &organization)
	if err != nil {
		return nil, fmt.Errorf("CreateOrganization: %s: %+v, name=%s, body=%s", err, organization, name, result.body)
	}

	return &organization, nil
}

// organizationIdsByName lists the ids of the organizations holding a name.
func (c client) organizationIdsByName(name string) ([]string, error) {
	organizations, err := c.GetOrganizations()
	if err != nil {
		return nil, err
	}

	var ids []string
	for _, organization := range organizations {
		if organization.Name == name {
			ids = append(ids, organization.ID)
		}
	}

	return ids, nil
}

func (c client) UpdateOrganization(id string, organization *Organization) error {
	jsonData, err := json.Marshal(organization)
	if err != nil {
		return fmt.Errorf("UpdateOrganization: Error on marshalling data: %s", err)
	}

	url := fmt.Sprintf("/organization/%s", id)
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("UpdateOrganization: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on updating the organization\nbody=%s", body)
	}

	return nil
}

func (c client) DeleteOrganization(id string) error {
	url := fmt.Sprintf("/organization/%s", id)
	req, err := http.NewRequest("DELETE", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DeleteOrganization: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on deleting the organization\nbody=%s", body)
	}

	return nil
}

func (c client) GetServer(id string) (*Server, error) {
	url := fmt.Sprintf("/server/%s", id)
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting the server\nbody=%s", body)
	}

	var server Server
	err = json.Unmarshal(body, &server)

	if err != nil {
		return nil, fmt.Errorf("GetServer: %s: %+v, id=%s, body=%s", err, server, id, body)
	}

	return &server, nil
}

func (c client) GetServers() ([]Server, error) {
	url := fmt.Sprintf("/server")
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetServers: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting servers\nbody=%s", body)
	}

	var servers []Server
	err = json.Unmarshal(body, &servers)

	if err != nil {
		return nil, fmt.Errorf("GetServers: %s: %+v, body=%s", err, servers, body)
	}

	return servers, nil
}

func (c client) CreateServer(serverData map[string]interface{}) (*Server, error) {
	serverStruct := Server{}

	if v, ok := serverData["name"]; ok {
		serverStruct.Name = v.(string)
	}
	if v, ok := serverData["protocol"]; ok {
		serverStruct.Protocol = v.(string)
	}
	if v, ok := serverData["cipher"]; ok {
		serverStruct.Cipher = v.(string)
	}
	if v, ok := serverData["network"]; ok {
		serverStruct.Network = v.(string)
	}
	if v, ok := serverData["hash"]; ok {
		serverStruct.Hash = v.(string)
	}
	if v, ok := serverData["port"]; ok {
		serverStruct.Port = v.(int)
	}
	if v, ok := serverData["bind_address"]; ok {
		serverStruct.BindAddress = v.(string)
	}
	if v, ok := serverData["groups"]; ok {
		groups := make([]string, 0)
		for _, group := range v.([]interface{}) {
			groups = append(groups, group.(string))
		}
		serverStruct.Groups = groups
	}
	if v, ok := serverData["dns_servers"]; ok {
		dnsServers := make([]string, 0)
		for _, dns := range v.([]interface{}) {
			dnsServers = append(dnsServers, dns.(string))
		}
		serverStruct.DnsServers = dnsServers
	}
	if v, ok := serverData["network_wg"]; ok {
		serverStruct.NetworkWG = v.(string)
	}
	if v, ok := serverData["port_wg"]; ok {
		serverStruct.PortWG = v.(int)
	}

	isWgEnabled := serverStruct.NetworkWG != "" && serverStruct.PortWG > 0
	serverStruct.WG = isWgEnabled

	if v, ok := serverData["sso_auth"]; ok {
		serverStruct.SsoAuth = v.(bool)
	}

	if v, ok := serverData["otp_auth"]; ok {
		serverStruct.OtpAuth = v.(bool)
	}

	if v, ok := serverData["device_auth"]; ok {
		serverStruct.DeviceAuth = v.(bool)
	}

	if v, ok := serverData["dynamic_firewall"]; ok {
		serverStruct.DynamicFirewall = v.(bool)
	}

	if v, ok := serverData["ipv6"]; ok {
		serverStruct.IPv6 = v.(bool)
	}

	if v, ok := serverData["dh_param_bits"]; ok {
		serverStruct.DhParamBits = v.(int)
	}

	if v, ok := serverData["ping_interval"]; ok {
		serverStruct.PingInterval = v.(int)
	}

	if v, ok := serverData["ping_timeout"]; ok {
		serverStruct.PingTimeout = v.(int)
	}

	if v, ok := serverData["link_ping_interval"]; ok {
		serverStruct.LinkPingInterval = v.(int)
	}

	if v, ok := serverData["link_ping_timeout"]; ok {
		serverStruct.LinkPingTimeout = v.(int)
	}

	if v, ok := serverData["session_timeout"]; ok {
		serverStruct.SessionTimeout = v.(int)
	}

	if v, ok := serverData["inactive_timeout"]; ok {
		serverStruct.InactiveTimeout = v.(int)
	}

	if v, ok := serverData["max_clients"]; ok {
		serverStruct.MaxClients = v.(int)
	}

	if v, ok := serverData["network_mode"]; ok {
		serverStruct.NetworkMode = v.(string)
	}

	if v, ok := serverData["network_start"]; ok {
		serverStruct.NetworkStart = v.(string)
	}

	if v, ok := serverData["network_end"]; ok {
		serverStruct.NetworkEnd = v.(string)
	}

	if serverStruct.NetworkMode == ServerNetworkModeBridge && (serverStruct.NetworkStart == "" || serverStruct.NetworkEnd == "") {
		return nil, fmt.Errorf("the attribute network_mode = %s requires network_start and network_end attributes", ServerNetworkModeBridge)
	}

	if v, ok := serverData["mss_fix"]; ok {
		serverStruct.MssFix = v.(int)
	}

	if v, ok := serverData["tun_mtu"]; ok {
		serverStruct.TunMtu = v.(int)
	}

	if v, ok := serverData["max_devices"]; ok {
		serverStruct.MaxDevices = v.(int)
	}

	if v, ok := serverData["pre_connect_msg"]; ok {
		serverStruct.PreConnectMsg = v.(string)
	}

	if v, ok := serverData["allowed_devices"]; ok {
		serverStruct.AllowedDevices = v.(string)
	}

	if v, ok := serverData["search_domain"]; ok {
		serverStruct.SearchDomain = v.(string)
	}

	if v, ok := serverData["replica_count"]; ok {
		serverStruct.ReplicaCount = v.(int)
	}

	if v, ok := serverData["multi_device"]; ok {
		serverStruct.MultiDevice = v.(bool)
	}

	if v, ok := serverData["debug"]; ok {
		serverStruct.Debug = v.(bool)
	}

	if v, ok := serverData["geo_sort"]; ok {
		serverStruct.GeoSort = v.(bool)
	}

	if v, ok := serverData["ovpn_dco"]; ok {
		serverStruct.OvpnDco = v.(bool)
	}

	if v, ok := serverData["restrict_routes"]; ok {
		serverStruct.RestrictRoutes = v.(bool)
	}

	if v, ok := serverData["block_outside_dns"]; ok {
		serverStruct.BlockOutsideDns = v.(bool)
	}

	if v, ok := serverData["dns_mapping"]; ok {
		serverStruct.DnsMapping = v.(bool)
	}

	if v, ok := serverData["inter_client"]; ok {
		serverStruct.InterClient = v.(bool)
	}

	if v, ok := serverData["vxlan"]; ok {
		serverStruct.VxLan = v.(bool)
	}

	jsonData, err := serverStruct.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("CreateServer: Error on marshalling data: %s", err)
	}

	// Pritunl takes any number of servers under one name, so a create that
	// failed transiently is recognised by the server that was not there before
	// it was sent and holds what it asked for
	known, err := c.serverIdsByName(serverStruct.Name)
	if err != nil {
		return nil, fmt.Errorf("CreateServer: %s", err)
	}

	var created *Server

	result, adopted, err := retryCreate(
		func() attemptResult {
			return c.post("/server", jsonData)
		},
		func() (bool, error) {
			servers, err := c.GetServers()
			if err != nil {
				return false, err
			}

			var live []string
			for _, server := range servers {
				if serverMatches(server, serverStruct) {
					live = append(live, server.ID)
				}
			}

			id, found, err := pickCreated("servers", serverStruct.Name, newIds(known, live))
			if err != nil || !found {
				return false, err
			}

			created, err = c.GetServer(id)

			return err == nil, err
		},
	)
	if err != nil {
		return nil, fmt.Errorf("CreateServer: %s", err)
	}
	if adopted {
		return created, nil
	}
	if result.err != nil {
		return nil, fmt.Errorf("CreateServer: Error on HTTP request: %s", result.err)
	}
	if result.status != 200 {
		return nil, fmt.Errorf("Non-200 response on creating the server\ncode=%d\nbody=%s", result.status, result.body)
	}

	var server Server
	err = json.Unmarshal(result.body, &server)
	if err != nil {
		return nil, fmt.Errorf("CreateServer: Error on unmarshalling http response: %s", err)
	}

	return &server, nil
}

// serverMatches reports whether a server of the instance is the one a create
// asked for: the name, and the network, the port and the protocol wherever the
// request fixed them (Pritunl picks them itself otherwise).
func serverMatches(live, asked Server) bool {
	return live.Name == asked.Name &&
		(asked.Network == "" || live.Network == asked.Network) &&
		(asked.Port == 0 || live.Port == asked.Port) &&
		(asked.Protocol == "" || live.Protocol == asked.Protocol)
}

// serverIdsByName lists the ids of the servers holding a name.
func (c client) serverIdsByName(name string) ([]string, error) {
	servers, err := c.GetServers()
	if err != nil {
		return nil, err
	}

	var ids []string
	for _, server := range servers {
		if server.Name == name {
			ids = append(ids, server.ID)
		}
	}

	return ids, nil
}

func (c client) UpdateServer(id string, server *Server) error {
	jsonData, err := server.MarshalJSON()
	if err != nil {
		return fmt.Errorf("UpdateServer: Error on marshalling data: %s", err)
	}

	url := fmt.Sprintf("/server/%s", id)
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("UpdateServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on updating the server\nbody=%s", body)
	}

	return nil
}

func (c client) DeleteServer(id string) error {
	url := fmt.Sprintf("/server/%s", id)
	req, err := http.NewRequest("DELETE", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DeleteServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on deleting the server\nbody=%s", body)
	}

	return nil
}

func (c client) GetOrganizationsByServer(serverId string) ([]Organization, error) {
	url := fmt.Sprintf("/server/%s/organization", serverId)
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GeteOrganizationsByServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting organizations on the server\nbody=%s", body)
	}

	var organizations []Organization
	json.Unmarshal(body, &organizations)

	return organizations, nil
}

func (c client) AttachOrganizationToServer(organizationId, serverId string) error {
	url := fmt.Sprintf("/server/%s/organization/%s", serverId, organizationId)
	req, err := http.NewRequest("PUT", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("AttachOrganizationToServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on attaching an organization the server\nbody=%s", body)
	}

	return nil
}

func (c client) DetachOrganizationFromServer(organizationId, serverId string) error {
	url := fmt.Sprintf("/server/%s/organization/%s", serverId, organizationId)
	req, err := http.NewRequest("DELETE", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DetachOrganizationFromServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on detaching the organization from the server\nbody=%s", body)
	}

	return nil
}

func (c client) StartServer(serverId string) error {
	url := fmt.Sprintf("/server/%s/operation/start", serverId)
	req, err := http.NewRequest("PUT", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("StartServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on starting the server\nbody=%s", body)
	}

	return nil
}

func (c client) StopServer(serverId string) error {
	url := fmt.Sprintf("/server/%s/operation/stop", serverId)
	req, err := http.NewRequest("PUT", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("StopServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on stopping the server\nbody=%s", body)
	}

	return nil
}

func (c client) GetRoutesByServer(serverId string) ([]Route, error) {
	url := fmt.Sprintf("/server/%s/route", serverId)
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetRoutesByServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting routes on the server\nbody=%s", body)
	}

	var routes []Route
	json.Unmarshal(body, &routes)

	return routes, nil
}

func (c client) AddRouteToServer(serverId string, route Route) error {
	return c.postRoutes("AddRouteToServer", serverId, fmt.Sprintf("/server/%s/route", serverId), route, []Route{route}, "adding a route to the server")
}

func (c client) AddRoutesToServer(serverId string, routes []Route) error {
	return c.postRoutes("AddRoutesToServer", serverId, fmt.Sprintf("/server/%s/routes", serverId), routes, routes, "adding routes to the server")
}

// postRoutes adds routes to a server. The call survives the web server
// restarting under it: after a transient failure the routes of the server are
// read back, and the request is sent again only for as long as some of the
// routes sent are missing or hold other values than the ones sent. Pritunl
// updates a route that already exists in place, so a repeated request never
// leaves a duplicate either way.
func (c client) postRoutes(operation, serverId, url string, payload interface{}, routes []Route, description string) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s: Error on marshalling data: %s", operation, err)
	}

	result, adopted, err := retryCreate(
		func() attemptResult {
			return c.post(url, jsonData)
		},
		func() (bool, error) {
			live, err := c.GetRoutesByServer(serverId)
			if err != nil {
				return false, err
			}

			return routesPresent(live, routes), nil
		},
	)
	if err != nil {
		return fmt.Errorf("%s: %s", operation, err)
	}
	if adopted {
		return nil
	}
	if result.err != nil {
		return fmt.Errorf("%s: Error on HTTP request: %s", operation, result.err)
	}
	if result.status != 200 {
		return fmt.Errorf("Non-200 response on %s\ncode=%d\nbody=%s", description, result.status, result.body)
	}

	return nil
}

// routesPresent reports whether every wanted route is among the live ones with
// the very values it was sent with.
func routesPresent(live, wanted []Route) bool {
	liveByNetwork := make(map[string]Route, len(live))
	for _, route := range live {
		liveByNetwork[route.Network] = route
	}

	for _, want := range wanted {
		got, found := liveByNetwork[want.Network]
		if !found || got.Nat != want.Nat || got.NetGateway != want.NetGateway || got.Comment != want.Comment {
			return false
		}
	}

	return true
}

// post sends one POST request, with the status and the body of the answer, or
// the transport error, handed back as they are.
func (c client) post(url string, jsonData []byte) attemptResult {
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return attemptResult{err: fatalError{err}}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return attemptResult{err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return attemptResult{err: err}
	}

	return attemptResult{status: resp.StatusCode, body: body}
}

func (c client) UpdateRouteOnServer(serverId string, route Route) error {
	jsonData, err := json.Marshal(route)

	url := fmt.Sprintf("/server/%s/route/%s", serverId, route.GetID())
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("UpdateRouteOnServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on updating a route on the server\nbody=%s", body)
	}

	return nil
}

func (c client) DeleteRouteFromServer(serverId string, route Route) error {
	url := fmt.Sprintf("/server/%s/route/%s", serverId, route.GetID())
	req, err := http.NewRequest("DELETE", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DeleteRouteFromServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	// Pritunl couples the default routes: deleting 0.0.0.0/0 removes its
	// ::/0 companion too, so the companion's own delete finds nothing.
	// A route already gone is a delete that succeeded.
	if resp.StatusCode == 404 {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on deleting a route on the server\nbody=%s", body)
	}

	return nil
}

func (c client) GetUser(id string, orgId string) (*User, error) {
	url := fmt.Sprintf("/user/%s/%s", orgId, id)
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetUser: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting the user\nbody=%s", body)
	}

	var user User
	err = json.Unmarshal(body, &user)
	if err != nil {
		return nil, fmt.Errorf("GetUser: %s: %+v, id=%s, body=%s", err, user, id, body)
	}

	return &user, nil
}

func (c client) CreateUser(newUser User) (*User, error) {
	jsonData, err := json.Marshal(newUser)
	if err != nil {
		return nil, fmt.Errorf("CreateUser: Error on marshalling data: %s", err)
	}

	known, err := c.userIdsByName(newUser.Organization, newUser.Name)
	if err != nil {
		return nil, fmt.Errorf("CreateUser: %s", err)
	}

	var created *User

	result, adopted, err := retryCreate(
		func() attemptResult {
			return c.post(fmt.Sprintf("/user/%s", newUser.Organization), jsonData)
		},
		func() (bool, error) {
			live, err := c.userIdsByName(newUser.Organization, newUser.Name)
			if err != nil {
				return false, err
			}

			id, found, err := pickCreated("users", newUser.Name, newIds(known, live))
			if err != nil || !found {
				return false, err
			}

			created, err = c.GetUser(id, newUser.Organization)

			return err == nil, err
		},
	)
	if err != nil {
		return nil, fmt.Errorf("CreateUser: %s", err)
	}
	if adopted {
		return created, nil
	}
	if result.err != nil {
		return nil, fmt.Errorf("CreateUser: Error on HTTP request: %s", result.err)
	}
	if result.status != 200 {
		return nil, fmt.Errorf("Non-200 response on creating the user\ncode=%d\nbody=%s", result.status, result.body)
	}

	var users []User
	err = json.Unmarshal(result.body, &users)
	if err != nil {
		return nil, fmt.Errorf("CreateUser: Error on unmarshalling API response %s (body=%+v)", err, string(result.body))
	}

	if len(users) > 0 {
		return &users[0], nil
	}

	return nil, fmt.Errorf("empty users response")
}

// userIdsByName lists the ids of the users of an organization holding a name.
func (c client) userIdsByName(organizationId, name string) ([]string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("/user/%s", organizationId), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Error on HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error on reading the users: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting the users of the organization\ncode=%d\nbody=%s", resp.StatusCode, body)
	}

	// only what tells a user apart is decoded: the list carries far more than
	// the user the provider manages
	var users []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &users); err != nil {
		return nil, fmt.Errorf("Error on unmarshalling the users: %s", err)
	}

	var ids []string
	for _, user := range users {
		if user.Name == name {
			ids = append(ids, user.ID)
		}
	}

	return ids, nil
}

func (c client) UpdateUser(id string, user *User) error {
	jsonData, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("UpdateUser: Error on marshalling data: %s", err)
	}

	url := fmt.Sprintf("/user/%s/%s", user.Organization, id)
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("UpdateUser: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on updating the user\nbody=%s", body)
	}

	return nil
}

func (c client) DeleteUser(id string, orgId string) error {
	url := fmt.Sprintf("/user/%s/%s", orgId, id)
	req, err := http.NewRequest("DELETE", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DeleteUser: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on deleting the user\nbody=%s", body)
	}

	return nil
}

func (c client) GetHosts() ([]Host, error) {
	url := fmt.Sprintf("/host")
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetHosts: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting the hosts\nbody=%s", body)
	}

	var hosts []Host

	err = json.Unmarshal(body, &hosts)
	if err != nil {
		return nil, fmt.Errorf("GetHosts: %s: %+v, body=%s", err, hosts, body)
	}

	return hosts, nil
}

func (c client) GetHostsByServer(serverId string) ([]Host, error) {
	url := fmt.Sprintf("/server/%s/host", serverId)
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetHostsByServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting hosts by the server\nbody=%s", body)
	}

	var hosts []Host

	err = json.Unmarshal(body, &hosts)
	if err != nil {
		return nil, fmt.Errorf("GetHostsByServer: %s: %+v, body=%s", err, hosts, body)
	}

	return hosts, nil
}

func (c client) AttachHostToServer(hostId, serverId string) error {
	url := fmt.Sprintf("/server/%s/host/%s", serverId, hostId)
	req, err := http.NewRequest("PUT", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("AttachHostToServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on attachhing the host the server\nbody=%s", body)
	}

	return nil
}

func (c client) DetachHostFromServer(hostId, serverId string) error {
	url := fmt.Sprintf("/server/%s/host/%s", serverId, hostId)
	req, err := http.NewRequest("DELETE", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DetachHostFromServer: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on detaching the host from the server\nbody=%s", body)
	}

	return nil
}

// PingWebServer tells whether the web server answers again after a restart.
// The request honors the context, so a listener that accepts a connection and
// then hangs cannot outlive the caller's deadline the way TestApiCall can.
// The error is returned wrapped, which lets the caller tell a TLS certificate
// verification failure apart: right after the settings resource replaced or
// reset the certificate, being presented one the client does not trust is
// itself proof the server is up and serving again.
func (c client) PingWebServer(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "/state", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PingWebServer: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("non-200 response from the web server\ncode=%d\nbody=%s\n", resp.StatusCode, body)
	}

	return nil
}

func (c client) GetSettings() (Settings, error) {
	url := "/settings"
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetSettings: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting the settings\ncode=%d", resp.StatusCode)
	}

	settings := Settings{}

	// a successful response carries the web server private key along with every
	// other secret of the instance, so neither the body nor the parsed object
	// are reported back on failure. Numbers are decoded as json.Number so that
	// the settings handed back to the API keep the representation it returned.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	err = decoder.Decode(&settings)
	if err != nil {
		return nil, fmt.Errorf("GetSettings: Error on unmarshalling response: %s", err)
	}

	return settings, nil
}

// UpdateSettings replaces the whole settings object of the instance. PUT
// /settings is a full replace, so the settings handed over here have to be the
// complete object read from GetSettings with the wanted changes applied on top
// of it, otherwise every setting missing from the body is cleared.
func (c client) UpdateSettings(settings Settings) error {
	settings.normalize()

	jsonData, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("UpdateSettings: Error on marshalling data: %s", err)
	}

	url := "/settings"
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("UpdateSettings: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		// a body the web server could not deserialise is answered with an empty
		// 400, so the status code is reported next to it
		return fmt.Errorf("Non-200 response on updating the settings\ncode=%d\nbody=%s", resp.StatusCode, body)
	}

	return nil
}

// GetAdministrators reads every administrator account of the instance. It also
// backs the lookup of an administrator by username, which is what makes an
// import possible without knowing the object id Pritunl gave it.
func (c client) GetAdministrators() ([]Administrator, error) {
	url := "/admin"
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetAdministrators: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, newApiError("getting the administrators", resp.StatusCode, body)
	}

	var administrators []Administrator

	if err = decodeAdministrators(body, &administrators); err != nil {
		return nil, fmt.Errorf("GetAdministrators: Error on unmarshalling response: %s", err)
	}

	return administrators, nil
}

// GetAdministrator reads a single administrator account.
//
// Pritunl has no answer of its own for an administrator that does not exist:
// the handler looks it up, gets nothing back and fails while building the
// response, which reaches the client as a plain 500 rather than as a 404. That
// is indistinguishable from an instance in trouble, so a failed read is
// checked against the list of administrators, which does answer properly, and
// only an id that is really gone yields ErrAdministratorNotFound.
func (c client) GetAdministrator(id string) (Administrator, error) {
	url := fmt.Sprintf("/admin/%s", id)
	req, err := http.NewRequest("GET", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetAdministrator: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		apiError := newApiError("getting the administrator", resp.StatusCode, body)

		administrators, listErr := c.GetAdministrators()
		if listErr != nil {
			return nil, apiError
		}

		for _, administrator := range administrators {
			if administrator.String("id") == id {
				return nil, apiError
			}
		}

		return nil, ErrAdministratorNotFound
	}

	administrator := Administrator{}

	if err = decodeAdministrators(body, &administrator); err != nil {
		return nil, fmt.Errorf("GetAdministrator: Error on unmarshalling response: %s", err)
	}

	return administrator, nil
}

// CreateAdministrator adds an administrator account. Unlike the update, the
// create takes a partial body without any risk: there is no existing account
// to preserve, and every field the request leaves out is defaulted to false by
// the backend.
func (c client) CreateAdministrator(administrator Administrator) (Administrator, error) {
	jsonData, err := json.Marshal(administrator)
	if err != nil {
		return nil, fmt.Errorf("CreateAdministrator: Error on marshalling data: %s", err)
	}

	// the username of an administrator is unique, so a create that failed
	// transiently is settled by the account being there, unless it was there
	// before the request was sent: that one is the answer of the API to give
	username := administrator.String("username")
	var created Administrator

	existing, err := c.GetAdministrators()
	if err != nil {
		return nil, fmt.Errorf("CreateAdministrator: %s", err)
	}
	for _, account := range existing {
		if username != "" && account.String("username") == username {
			username = ""
		}
	}

	result, adopted, err := retryCreate(
		func() attemptResult {
			return c.post("/admin", jsonData)
		},
		func() (bool, error) {
			if username == "" {
				return false, nil
			}

			live, err := c.GetAdministrators()
			if err != nil {
				return false, err
			}

			for _, account := range live {
				if account.String("username") == username {
					created = account

					return true, nil
				}
			}

			return false, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("CreateAdministrator: %s", err)
	}
	if adopted {
		return created, nil
	}
	if result.err != nil {
		return nil, fmt.Errorf("CreateAdministrator: Error on HTTP request: %s", result.err)
	}
	if result.status != 200 {
		return nil, newApiError("creating the administrator", result.status, result.body)
	}

	created = Administrator{}

	if err = decodeAdministrators(result.body, &created); err != nil {
		return nil, fmt.Errorf("CreateAdministrator: Error on unmarshalling response: %s", err)
	}

	return created, nil
}

func (c client) UpdateAdministrator(id string, administrator Administrator) error {
	administrator.normalize()

	jsonData, err := json.Marshal(administrator)
	if err != nil {
		return fmt.Errorf("UpdateAdministrator: Error on marshalling data: %s", err)
	}

	url := fmt.Sprintf("/admin/%s", id)
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("UpdateAdministrator: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return newApiError("updating the administrator", resp.StatusCode, body)
	}

	return nil
}

// DeleteAdministrator removes an administrator account for good, it is not a
// soft disable. Pritunl refuses to remove the last super user of the instance
// with a no_admins error, which is what keeps a destroy from locking everyone
// out of the web console.
func (c client) DeleteAdministrator(id string) error {
	url := fmt.Sprintf("/admin/%s", id)
	req, err := http.NewRequest("DELETE", url, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DeleteAdministrator: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return newApiError("deleting the administrator", resp.StatusCode, body)
	}

	return nil
}

// decodeAdministrators decodes an administrator response into the raw objects
// the round trip hands back, keeping numbers as json.Number so that a field
// this provider does not model is written back exactly as it was read.
func decodeAdministrators(body []byte, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	return decoder.Decode(target)
}

func (c client) GetSubscription() (Subscription, error) {
	var subscription Subscription

	req, err := http.NewRequest("GET", "/subscription", nil)
	if err != nil {
		return subscription, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return subscription, fmt.Errorf("GetSubscription: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return subscription, fmt.Errorf("Non-200 response on getting the subscription\ncode=%d\nbody=%s", resp.StatusCode, body)
	}

	err = json.Unmarshal(body, &subscription)
	if err != nil {
		return subscription, fmt.Errorf("GetSubscription: %s", err)
	}

	return subscription, nil
}

func (c client) ActivateSubscription(license string) error {
	jsonData, err := json.Marshal(SubscriptionActivation{License: license})
	if err != nil {
		return err
	}

	// activating the same license again changes nothing, so a request the
	// restart cut short is simply sent again
	result, _, err := retryCreate(
		func() attemptResult {
			return c.post("/subscription", jsonData)
		},
		func() (bool, error) {
			return false, nil
		},
	)
	if err != nil {
		return err
	}
	if result.err != nil {
		return fmt.Errorf("ActivateSubscription: Error on HTTP request: %s", result.err)
	}
	if result.status != 200 {
		// Pritunl validates the key against its subscription server and
		// answers an invalid or unreachable one with a json error document
		// (e.g. 470 license_invalid), reported here verbatim.
		return fmt.Errorf("Non-200 response on activating the subscription\ncode=%d\nbody=%s", result.status, result.body)
	}

	return nil
}

func (c client) DeleteSubscription() error {
	req, err := http.NewRequest("DELETE", "/subscription", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DeleteSubscription: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Non-200 response on deleting the subscription\ncode=%d\nbody=%s", resp.StatusCode, body)
	}

	return nil
}

func (c client) GetHost(id string) (*Host, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("/host/%s", id), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetHost: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil
	}

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on getting the host\ncode=%d\nbody=%s", resp.StatusCode, body)
	}

	var host Host
	if err := json.Unmarshal(body, &host); err != nil {
		return nil, fmt.Errorf("GetHost: %s", err)
	}

	return &host, nil
}

func (c client) UpdateHost(id string, host HostUpdate) (*Host, error) {
	jsonData, err := json.Marshal(host)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("PUT", fmt.Sprintf("/host/%s", id), bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("UpdateHost: Error on HTTP request: %s", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Non-200 response on updating the host\ncode=%d\nbody=%s", resp.StatusCode, body)
	}

	var updated Host
	if err := json.Unmarshal(body, &updated); err != nil {
		return nil, fmt.Errorf("UpdateHost: %s", err)
	}

	return &updated, nil
}

func NewClient(baseUrl, apiToken, apiSecret string, insecure bool) Client {
	underlyingTransport := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
	}
	httpClient := &http.Client{
		Transport: &transport{
			baseUrl:             baseUrl,
			apiToken:            apiToken,
			apiSecret:           apiSecret,
			underlyingTransport: underlyingTransport,
		},
	}

	return &client{httpClient: httpClient}
}
