package api

import (
	"errors"
	"net/http"

	"mhvtl-control/internal/rawdisk"
)

// mvtlDTO is the decoded MVTL superblock summary shared by the disk listing
// and the format response.
type mvtlDTO struct {
	Barcode            string `json:"barcode"`
	VendorID           string `json:"vendor_id"`
	ProductID          string `json:"product_id"`
	TotalCapacityBytes uint64 `json:"total_capacity_bytes"`
	GenerationID       uint64 `json:"generation_id"`
	NumPartitions      uint32 `json:"num_partitions"`
	Version            uint16 `json:"version"`
	LogicalSector      uint16 `json:"logical_sector"`
}

// diskDTO is a candidate block device with its optional MVTL superblock.
type diskDTO struct {
	Name               string   `json:"name"`
	Path               string   `json:"path"`
	SizeBytes          uint64   `json:"size_bytes"`
	LogicalSectorSize  int      `json:"logical_sector_size"`
	PhysicalSectorSize int      `json:"physical_sector_size"`
	MVTLFormatted      bool     `json:"mvtl_formatted"`
	Status             string   `json:"status"`
	Model              string   `json:"model"`
	Serial             string   `json:"serial"`
	TempCelsius        *int     `json:"temp_celsius"`
	MVTL               *mvtlDTO `json:"mvtl"`
}

func toMVTLDTO(sb *rawdisk.Superblock) *mvtlDTO {
	if sb == nil {
		return nil
	}
	return &mvtlDTO{
		Barcode:            sb.Barcode,
		VendorID:           sb.VendorID,
		ProductID:          sb.ProductID,
		TotalCapacityBytes: sb.TotalCapacityBytes,
		GenerationID:       sb.GenerationID,
		NumPartitions:      sb.NumPartitions,
		Version:            sb.Version,
		LogicalSector:      sb.LogicalSector,
	}
}

func toDiskDTO(d rawdisk.Disk) diskDTO {
	return diskDTO{
		Name:               d.Name,
		Path:               d.Path,
		SizeBytes:          d.SizeBytes,
		LogicalSectorSize:  d.LogicalSectorSize,
		PhysicalSectorSize: d.PhysicalSectorSize,
		MVTLFormatted:      d.MVTLFormatted,
		Status:             d.Status,
		Model:              d.Model,
		Serial:             d.Serial,
		TempCelsius:        d.TempCelsius,
		MVTL:               toMVTLDTO(d.MVTL),
	}
}

func (s *Server) handleDisks(w http.ResponseWriter, _ *http.Request) {
	disks, err := rawdisk.ListDisks(s.cfg.DiskOptions)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "disk_error", err.Error())
		return
	}
	out := make([]diskDTO, 0, len(disks))
	for _, d := range disks {
		out = append(out, toDiskDTO(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"disks": out})
}

type formatHTTPRequest struct {
	Device        string `json:"device"`
	Barcode       string `json:"barcode"`
	Vendor        string `json:"vendor"`
	Product       string `json:"product"`
	CapacityBytes uint64 `json:"capacity_bytes"`
	Force         bool   `json:"force"`
}

func (s *Server) handleFormat(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Formatter == nil {
		writeError(w, http.StatusServiceUnavailable, "formatter_unavailable", "raw-disk formatter unavailable")
		return
	}
	var req formatHTTPRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sb, err := s.cfg.Formatter.Format(rawdisk.FormatRequest{
		Device:        req.Device,
		Barcode:       req.Barcode,
		Vendor:        req.Vendor,
		Product:       req.Product,
		CapacityBytes: req.CapacityBytes,
		Force:         req.Force,
	})
	if err != nil {
		if errors.Is(err, rawdisk.ErrAlreadyFormatted) {
			writeError(w, http.StatusConflict, "already_formatted", err.Error())
			return
		}
		if errors.Is(err, rawdisk.ErrDeviceInUse) {
			writeError(w, http.StatusConflict, "device_in_use", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "format_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toMVTLDTO(sb))
}
