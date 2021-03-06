package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sync"

	"github.com/libopenstorage/openstorage/api"
	"github.com/libopenstorage/openstorage/api/spec"
	"github.com/libopenstorage/openstorage/config"
	"github.com/libopenstorage/openstorage/pkg/grpcserver"
	"github.com/libopenstorage/openstorage/pkg/options"
	"github.com/libopenstorage/openstorage/pkg/util"
	"github.com/libopenstorage/openstorage/volume"
	"github.com/libopenstorage/openstorage/volume/drivers"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	// VolumeDriver is the string returned in the handshake protocol.
	VolumeDriver = "VolumeDriver"
)

// Implementation of the Docker volumes plugin specification.
type driver struct {
	restBase
	spec.SpecHandler

	sdkUds string
	conn   *grpc.ClientConn
	mu     sync.Mutex
}

type handshakeResp struct {
	Implements []string
}

type volumeRequest struct {
	Name string
	Opts map[string]string
}

type mountRequest struct {
	Name string
	ID   string
}

type volumeResponse struct {
	Err string
}

type volumePathResponse struct {
	Mountpoint string
	volumeResponse
}

type volumeInfo struct {
	Name       string
	Mountpoint string
}

type capabilities struct {
	Scope string
}

type capabilitiesResponse struct {
	Capabilities capabilities
}

func newVolumePlugin(name, sdkUds string) restServer {
	d := &driver{
		restBase:    restBase{name: name, version: "0.3"},
		SpecHandler: spec.NewSpecHandler(),
		sdkUds:      sdkUds,
	}
	return d
}

func (d *driver) String() string {
	return d.name
}

func volDriverPath(method string) string {
	return fmt.Sprintf("/%s.%s", VolumeDriver, method)
}

func (d *driver) volNotFound(request string, id string, e error, w http.ResponseWriter) error {
	err := fmt.Errorf("Failed to locate volume: " + e.Error())
	if e == volume.ErrDriverInitializing {
		d.logRequest(request, id).Warnln(http.StatusInternalServerError, " ", err.Error())
	} else {
		d.logRequest(request, id).Warnln(http.StatusNotFound, " ", err.Error())
	}
	return err
}

func (d *driver) volNotMounted(request string, id string) error {
	err := fmt.Errorf("volume not mounted")
	d.logRequest(request, id).Debugln(http.StatusNotFound, " ", err.Error())
	return err
}

func (d *driver) Routes() []*Route {
	return []*Route{
		{verb: "POST", path: volDriverPath("Create"), fn: d.create},
		{verb: "POST", path: volDriverPath("Remove"), fn: d.remove},
		{verb: "POST", path: volDriverPath("Mount"), fn: d.mount},
		{verb: "POST", path: volDriverPath("Path"), fn: d.path},
		{verb: "POST", path: volDriverPath("List"), fn: d.list},
		{verb: "POST", path: volDriverPath("Get"), fn: d.get},
		{verb: "POST", path: volDriverPath("Unmount"), fn: d.unmount},
		{verb: "POST", path: volDriverPath("Capabilities"), fn: d.capabilities},
		{verb: "POST", path: "/Plugin.Activate", fn: d.handshake},
		{verb: "GET", path: "/status", fn: d.status},
	}
}

func (d *driver) emptyResponse(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(&volumeResponse{})
}

func (d *driver) errorResponse(method string, w http.ResponseWriter, err error) {
	if err == volume.ErrDriverInitializing {
		d.sendError(method, "", w, err.Error(), http.StatusInternalServerError)
	} else {
		json.NewEncoder(w).Encode(&volumeResponse{Err: err.Error()})
	}
}

func (d *driver) volFromName(name string) (*api.Volume, error) {
	v, err := volumedrivers.Get(d.name)
	if err != nil {
		return nil, fmt.Errorf("Cannot locate volume driver for %s: %s", d.name, err.Error())
	}
	return util.VolumeFromName(v, name)
}

func (d *driver) volFromNameSdk(ctx context.Context, volumes api.OpenStorageVolumeClient, name string) (*api.Volume, error) {
	// get volume id
	volId, err := d.volIdFromName(ctx, volumes, name)
	if err != nil {
		return nil, err
	}

	// inspect for actual volume
	inspectResp, err := volumes.Inspect(ctx, &api.SdkVolumeInspectRequest{
		VolumeId: volId,
	})
	if err == nil {
		return inspectResp.Volume, nil
	}
	return nil, fmt.Errorf("Cannot locate volume with name %s", name)
}

func (d *driver) volIdFromName(ctx context.Context, volumes api.OpenStorageVolumeClient, name string) (string, error) {
	enumerateResp, err := volumes.EnumerateWithFilters(ctx, &api.SdkVolumeEnumerateWithFiltersRequest{
		Locator: &api.VolumeLocator{
			Name: name,
		},
	})
	if err != nil {
		return "", err
	} else if len(enumerateResp.VolumeIds) < 1 {
		return "", fmt.Errorf("Cannot locate volume with name %s", name)
	}

	return enumerateResp.VolumeIds[0], nil
}

func (d *driver) decode(method string, w http.ResponseWriter, r *http.Request) (*volumeRequest, error) {
	var request volumeRequest
	err := json.NewDecoder(r.Body).Decode(&request)
	if err != nil {
		e := fmt.Errorf("Unable to decode JSON payload")
		d.sendError(method, "", w, e.Error()+":"+err.Error(), http.StatusBadRequest)
		return nil, e
	}
	d.logRequest(method, request.Name).Debugln("")
	return &request, nil
}

func (d *driver) decodeMount(method string, w http.ResponseWriter, r *http.Request) (*mountRequest, error) {
	var request mountRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		e := fmt.Errorf("Unable to decode JSON payload")
		d.sendError(method, "", w, e.Error()+":"+err.Error(), http.StatusBadRequest)
		return nil, e
	}
	d.logRequest(method, request.Name).Debugf("ID: %v", request.ID)
	return &request, nil
}

func (d *driver) handshake(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(&handshakeResp{
		[]string{VolumeDriver},
	})
	if err != nil {
		d.sendError("handshake", "", w, "encode error", http.StatusInternalServerError)
		return
	}
	d.logRequest("handshake", "").Debugln("Handshake completed")
}

func (d *driver) attachToken(ctx context.Context, request *volumeRequest) context.Context {
	token, tokenInName := d.GetTokenFromString(request.Name)
	if !tokenInName {
		token = request.Opts[api.Token]
	}
	md := metadata.New(map[string]string{
		"authorization": "bearer " + token,
	})
	return metadata.NewOutgoingContext(ctx, md)
}

func (d *driver) status(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, fmt.Sprintln("osd plugin", d.version))
}

func (d *driver) mountpath(name string) string {
	return path.Join(volume.MountBase, name)
}

func (d *driver) getConn() (*grpc.ClientConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		var err error
		d.conn, err = grpcserver.Connect(
			d.sdkUds,
			[]grpc.DialOption{grpc.WithInsecure()})
		if err != nil {
			return nil, fmt.Errorf("Failed to connect to gRPC handler: %v", err)
		}
	}
	return d.conn, nil
}

func (d *driver) create(w http.ResponseWriter, r *http.Request) {
	method := "create"
	ctx := r.Context()

	// decode request
	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}

	// attach token in context metadata
	ctx = d.attachToken(ctx, request)

	// get spec for volume creation
	specParsed, spec, locator, source, name := d.SpecFromString(request.Name)
	d.logRequest(method, name).Infoln("")
	if !specParsed {
		spec, locator, source, err = d.SpecFromOpts(request.Opts)
		if err != nil {
			d.errorResponse(method, w, err)
			return
		}
	}

	// get grpc connection
	conn, err := d.getConn()
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}

	// clone if exists, create otherwise
	spec.VolumeLabels = locator.VolumeLabels
	volumes := api.NewOpenStorageVolumeClient(conn)
	if source != nil && len(source.Parent) != 0 {
		// clone
		_, err = volumes.Clone(ctx, &api.SdkVolumeCloneRequest{
			Name:     name,
			ParentId: source.Parent,
		})
	} else {
		// create
		_, err = volumes.Create(ctx, &api.SdkVolumeCreateRequest{
			Name: name,
			Spec: spec,
		})
	}
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}

	json.NewEncoder(w).Encode(&volumeResponse{})
}

func (d *driver) remove(w http.ResponseWriter, r *http.Request) {
	method := "remove"
	ctx := r.Context()

	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}

	// attach token in context metadata
	ctx = d.attachToken(ctx, request)

	// get name for deletion
	_, _, _, _, name := d.SpecFromString(request.Name)
	d.logRequest(method, name).Infoln("")

	// get grpc connection
	conn, err := d.getConn()
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}
	volumes := api.NewOpenStorageVolumeClient(conn)

	// get volume id to delete
	volId, err := d.volIdFromName(ctx, volumes, name)
	if err != nil {
		notFound(w, r)
		return
	}

	// delete volume
	_, err = volumes.Delete(ctx, &api.SdkVolumeDeleteRequest{
		VolumeId: volId,
	})
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}

	json.NewEncoder(w).Encode(&volumeResponse{})
}

func (d *driver) scaleUp(
	method string,
	vd volume.VolumeDriver,
	inVol *api.Volume,
	allVols []*api.Volume,
	attachOptions map[string]string,
) (
	outVol *api.Volume,
	err error,
) {
	// Create new volume if existing volumes are not available.
	spec := inVol.Spec.Copy()
	spec.Scale = 1
	spec.ReplicaSet = nil
	volCount := len(allVols)
	for i := len(allVols); volCount < int(inVol.Spec.Scale); i++ {
		name := fmt.Sprintf("%s_%03d", inVol.Locator.Name, i)
		id := ""
		if id, err = vd.Create(
			&api.VolumeLocator{Name: name},
			nil,
			spec,
		); err != nil {
			// It is possible to get an error on a name conflict
			// either due to concurrent creates or holes punched in
			// from previous deletes.
			if err == volume.ErrExist {
				continue
			}
			return nil, err
		}
		if outVol, err = d.volFromName(id); err != nil {
			return nil, err
		}
		if _, err = vd.Attach(outVol.Id, attachOptions); err == nil {
			return outVol, nil
		}
		// If we fail to attach the volume, continue to look for a
		// free volume.
		volCount++
	}
	return nil, volume.ErrVolAttachedScale
}

func (d *driver) attachScale(
	method string,
	vd volume.VolumeDriver,
	inVol *api.Volume,
	attachOptions map[string]string,
) (
	*api.Volume,
	error,
) {
	// Find a volume that has data local to this node.
	vols, err := vd.Enumerate(
		&api.VolumeLocator{
			Name: fmt.Sprintf("%s.*", inVol.Locator.Name),
			VolumeLabels: map[string]string{
				volume.LocationConstraint: volume.LocalNode,
			},
		},
		nil,
	)
	// Try to attach local volumes.
	if err == nil {
		for _, vol := range vols {
			if v, err := d.attachVol(method, vd, vol, attachOptions); err == nil {
				return v, nil
			}
		}
	}
	// Create a new local volume if we fail to attach existing local volume
	// or if none exist.
	allVols, err := vd.Enumerate(
		&api.VolumeLocator{
			Name: fmt.Sprintf("%s.*", inVol.Locator.Name),
		},
		nil,
	)

	// Try to attach existing volumes.
	for _, outVol := range allVols {
		if _, err = vd.Attach(outVol.Id, attachOptions); err == nil {
			return outVol, nil
		}
	}

	if len(allVols) < int(inVol.Spec.Scale) {
		name := fmt.Sprintf("%s_%03d", inVol.Locator.Name, len(allVols))
		spec := inVol.Spec.Copy()
		spec.ReplicaSet = &api.ReplicaSet{Nodes: []string{volume.LocalNode}}
		spec.Scale = 1
		id, err := vd.Create(&api.VolumeLocator{Name: name}, nil, spec)
		if err != nil {
			return d.scaleUp(method, vd, inVol, allVols, attachOptions)
		}
		outVol, err := d.volFromName(id)
		if err != nil {
			return nil, err
		}
		if _, err = vd.Attach(outVol.Id, attachOptions); err == nil {
			return outVol, nil
		}
		// We failed to attach, scaleUp.
		allVols = append(allVols, outVol)
	}
	return d.scaleUp(method, vd, inVol, allVols, attachOptions)
}

func (d *driver) attachVol(
	method string,
	vd volume.VolumeDriver,
	vol *api.Volume,
	attachOptions map[string]string,
) (
	outVolume *api.Volume,
	err error,
) {
	attachPath, err := vd.Attach(vol.Id, attachOptions)

	switch err {
	case nil:
		d.logRequest(method, vol.Locator.Name).Debugf(
			"response %v", attachPath)
		return vol, nil
	case volume.ErrVolAttachedOnRemoteNode:
		d.logRequest(method, vol.Locator.Name).Infof(
			"Mount volume attached on remote node.")
		return vol, err
	default:
		d.logRequest(method, vol.Locator.Name).Warnf(
			"Cannot attach volume: %v", err.Error())
		return vol, err
	}
}

func (d *driver) attachOptionsFromSpec(
	spec *api.VolumeSpec,
) map[string]string {
	if spec.Passphrase != "" {
		opts := make(map[string]string)
		opts[options.OptionsSecret] = spec.Passphrase
		return opts
	}
	return nil
}

func (d *driver) mount(w http.ResponseWriter, r *http.Request) {
	var response volumePathResponse
	method := "mount"

	v, err := volumedrivers.Get(d.name)
	if err != nil {
		d.logRequest(method, "").Warnf("Cannot locate volume driver")
		d.errorResponse(method, w, err)
		return
	}

	request, err := d.decodeMount(method, w, r)
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}
	_, spec, _, _, name := d.SpecFromString(request.Name)
	attachOptions := d.attachOptionsFromSpec(spec)
	vol, err := d.volFromName(name)
	if err != nil {
		d.sendError(method, "", w, err.Error(), http.StatusBadRequest)
		return
	}

	// If a scaled volume is already mounted, check if it can be unmounted and
	// detached. If not return an error.
	mountpoint := d.mountpath(name)
	if vol.Spec.Scale > 1 {
		id := v.MountedAt(mountpoint)
		if len(id) != 0 {
			err = v.Unmount(id, mountpoint, nil)
			if err != nil {
				d.logRequest(method, "").Warnf("Error unmounting scaled volume: %v", err)
				err = fmt.Errorf("Cannot remount scaled volume(%v)."+
					" Volume %v is mounted at %v", name, id, mountpoint)
				d.errorResponse(method, w, err)
				return
			}

			if v.Type() == api.DriverType_DRIVER_TYPE_BLOCK {
				err = v.Detach(id, nil)
				if err != nil {
					d.logRequest(method, "").Warnf("Error detaching scaled volume: %v", err)
					mountErr := v.Mount(id, mountpoint, nil)
					if mountErr != nil {
						d.logRequest(method, "").Warnf("Error remounting scaled volume: %v", mountErr.Error())
					}
					err = fmt.Errorf("Cannot remount scaled volume(%v)."+
						" Volume %v is mounted at %v", name, id, mountpoint)
					d.logRequest(method, "").Warnf(err.Error())
					d.errorResponse(method, w, err)
					return
				}
			}
		}
	}

	// If this is a block driver, first attach the volume.
	if v.Type() == api.DriverType_DRIVER_TYPE_BLOCK {
		// If volume is scaled up, a new volume is created and
		// vol will change.
		if vol.Scaled() {
			vol, err = d.attachScale(method, v, vol, attachOptions)
		} else {
			vol, err = d.attachVol(method, v, vol, attachOptions)
		}
		if err != nil {
			d.errorResponse(method, w, err)
			return
		}
	}

	// Note that name is unchanged even if a new volume was created as a
	// result of scale up.
	response.Mountpoint = mountpoint
	os.MkdirAll(mountpoint, 0755)
	err = v.Mount(vol.Id, response.Mountpoint, nil)
	if err != nil {
		d.logRequest(method, request.Name).Warnf(
			"Cannot mount volume %v, %v",
			response.Mountpoint, err)
		d.errorResponse(method, w, err)
		return
	}
	d.logRequest(method, request.Name).Infof("response %v", response.Mountpoint)
	json.NewEncoder(w).Encode(&response)
}

func (d *driver) path(w http.ResponseWriter, r *http.Request) {
	method := "path"
	ctx := r.Context()
	var response volumePathResponse

	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}

	// attach token in context metadata
	ctx = d.attachToken(ctx, request)

	// get grpc connection
	conn, err := d.getConn()
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}

	volumes := api.NewOpenStorageVolumeClient(conn)
	_, _, _, _, name := d.SpecFromString(request.Name)
	vol, err := d.volFromNameSdk(ctx, volumes, name)
	if err != nil {
		e := d.volNotFound(method, request.Name, err, w)
		d.errorResponse(method, w, e)
		return
	}

	d.logRequest(method, name).Debugf("")
	if len(vol.AttachPath) == 0 || len(vol.AttachPath) == 0 {
		e := d.volNotMounted(method, name)
		d.errorResponse(method, w, e)
		return
	}
	response.Mountpoint = vol.AttachPath[0]
	response.Mountpoint = path.Join(response.Mountpoint, config.DataDir)
	d.logRequest(method, request.Name).Debugf("response %v", response.Mountpoint)
	json.NewEncoder(w).Encode(&response)
}

func (d *driver) list(w http.ResponseWriter, r *http.Request) {
	method := "list"
	ctx := r.Context()

	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}

	// attach token in context metadata
	ctx = d.attachToken(ctx, request)

	// get grpc connection
	conn, err := d.getConn()
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}

	// get all volumes
	volumes := api.NewOpenStorageVolumeClient(conn)
	enumerateResp, err := volumes.Enumerate(ctx, &api.SdkVolumeEnumerateRequest{})
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}

	volInfo := make([]volumeInfo, len(enumerateResp.VolumeIds))
	for i, id := range enumerateResp.VolumeIds {
		inspectResp, err := volumes.Inspect(ctx, &api.SdkVolumeInspectRequest{
			VolumeId: id,
		})
		if err != nil {
			continue
		}
		volInfo[i].Name = inspectResp.Volume.Locator.Name
		if len(inspectResp.Volume.AttachPath) > 0 || len(inspectResp.Volume.AttachPath) > 0 {
			volInfo[i].Mountpoint = path.Join(inspectResp.Volume.AttachPath[0], config.DataDir)
		}
	}
	json.NewEncoder(w).Encode(map[string][]volumeInfo{"Volumes": volInfo})
}

func (d *driver) get(w http.ResponseWriter, r *http.Request) {
	method := "get"
	ctx := r.Context()

	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}

	// attach token in context metadata
	ctx = d.attachToken(ctx, request)

	// get name from the request
	parsed, _, _, _, name := d.SpecFromString(request.Name)
	var returnName string
	if parsed {
		returnName = request.Name
	} else {
		returnName = name
	}

	// get grpc connection
	conn, err := d.getConn()
	if err != nil {
		d.errorResponse(method, w, err)
		return
	}

	// get volume
	volumes := api.NewOpenStorageVolumeClient(conn)
	vol, err := d.volFromNameSdk(ctx, volumes, name)
	if err != nil {
		e := d.volNotFound(method, request.Name, err, w)
		d.errorResponse(method, w, e)
		return
	}

	// create response info
	volInfo := volumeInfo{Name: returnName}
	if len(vol.AttachPath) > 0 || len(vol.AttachPath) > 0 {
		volInfo.Mountpoint = path.Join(vol.AttachPath[0], config.DataDir)
	}

	json.NewEncoder(w).Encode(map[string]volumeInfo{"Volume": volInfo})
}

func (d *driver) unmount(w http.ResponseWriter, r *http.Request) {
	method := "unmount"

	v, err := volumedrivers.Get(d.name)
	if err != nil {
		d.logRequest(method, "").Warnf(
			"Cannot locate volume driver: %v",
			err.Error())
		d.errorResponse(method, w, err)
		return
	}

	request, err := d.decodeMount(method, w, r)
	if err != nil {
		return
	}

	_, _, _, _, name := d.SpecFromString(request.Name)
	vol, err := d.volFromName(name)
	if err != nil {
		e := d.volNotFound(method, name, err, w)
		d.errorResponse(method, w, e)
		return
	}

	mountpoint := d.mountpath(name)
	id := vol.Id
	if vol.Spec.Scale > 1 {
		id = v.MountedAt(mountpoint)
		if len(id) == 0 {
			err := fmt.Errorf("Failed to find volume mapping for %v",
				mountpoint)
			d.logRequest(method, request.Name).Warnf(
				"Cannot unmount volume %v, %v",
				mountpoint, err)
			d.errorResponse(method, w, err)
			return
		}
	}

	opts := make(map[string]string)
	opts[options.OptionsDeleteAfterUnmount] = "true"

	err = v.Unmount(id, mountpoint, opts)
	if err != nil {
		d.logRequest(method, request.Name).Warnf(
			"Cannot unmount volume %v, %v",
			mountpoint, err)
		d.errorResponse(method, w, err)
		return
	}

	if v.Type() == api.DriverType_DRIVER_TYPE_BLOCK {
		_ = v.Detach(id, nil)
	}
	d.emptyResponse(w)
}

func (d *driver) capabilities(w http.ResponseWriter, r *http.Request) {
	method := "capabilities"
	var response capabilitiesResponse

	response.Capabilities.Scope = "global"
	d.logRequest(method, "").Infof("response %v", response.Capabilities.Scope)
	json.NewEncoder(w).Encode(&response)
}
