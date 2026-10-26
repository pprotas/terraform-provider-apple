// Copyright (c) AO Studio
// SPDX-License-Identifier: MPL-2.0

package apple

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/AhmedOsman00/terraform-provider-apple/internal/apple/models"
)

// AppTerritoryAvailability is one storefront to make an app available in.
//
// It is an input helper rather than a wire type, for the same reason
// AppManualPrice is: the request sends each territory twice, once as a
// placeholder in the relationship and once inline in "included".
type AppTerritoryAvailability struct {
	Territory       string
	Available       *bool
	ReleaseDate     *string
	PreOrderEnabled *bool
}

// GetAppAvailability reads which storefronts an app is sold in.
//
// This is Apple's AppAvailabilityV2, a different and richer resource from the
// in-app purchase and subscription availabilities: where those carry a flat
// list of territory codes, this carries a record per storefront with its own
// release date and pre-order settings.
func (c *Client) GetAppAvailability(appID string) (*models.AppAvailability, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/v1/apps/%s/appAvailabilityV2", c.HostURL, appID), nil)
	if err != nil {
		return nil, err
	}

	body, err := c.doRequest(req, nil)
	if err != nil {
		return nil, err
	}

	response := models.Response[models.AppAvailability]{}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	// An app whose availability has never been set answers 200 with a null
	// data member rather than 404.
	if response.Data.ID == "" {
		return nil, fmt.Errorf("availability not found for app '%s'", appID)
	}

	return &response.Data, nil
}

// GetAppTerritoryAvailabilities lists the per-storefront records behind an
// availability.
//
// The list runs to every storefront Apple sells in, so it is paginated like any
// other collection rather than read from the relationship linkage.
// include=territory populates each record's territory, without which the
// three-letter code -- the only part a configuration names -- is unreadable.
func (c *Client) GetAppTerritoryAvailabilities(availabilityID string) ([]models.TerritoryAvailability, error) {
	params := url.Values{}
	params.Set("include", "territory")

	return getAllPagesQuery[models.TerritoryAvailability](
		c,
		fmt.Sprintf("/v2/appAvailabilities/%s/territoryAvailabilities", availabilityID),
		defaultPageSize,
		params,
	)
}

// GetAppAvailabilityWithTerritories reads an app's availability and the
// storefronts behind it in one call.
func (c *Client) GetAppAvailabilityWithTerritories(appID string) (*models.AppAvailability, []models.TerritoryAvailability, error) {
	availability, err := c.GetAppAvailability(appID)
	if err != nil {
		return nil, nil, err
	}

	territories, err := c.GetAppTerritoryAvailabilities(availability.ID)
	if err != nil {
		return nil, nil, err
	}

	return availability, territories, nil
}

// SetAppAvailability creates availability or reconciles existing storefronts.
func (c *Client) SetAppAvailability(appID string, newTerritories bool, desired []AppTerritoryAvailability, authToken *string) (*models.AppAvailability, error) {
	availability, err := c.GetAppAvailability(appID)
	if err != nil {
		if strings.Contains(err.Error(), "availability not found for app") {
			return c.CreateAppAvailability(appID, newTerritories, desired, authToken)
		}
		return nil, err
	}
	if availability.Attributes.AvailableInNewTerritories == nil || *availability.Attributes.AvailableInNewTerritories != newTerritories {
		return nil, fmt.Errorf("availableInNewTerritories cannot be updated through Apple's published API; change it in App Store Connect first")
	}
	current, err := c.GetAppTerritoryAvailabilities(availability.ID)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]models.TerritoryAvailability, len(current))
	for _, territory := range current {
		if territory.Relationships == nil || territory.Relationships.Territory == nil {
			return nil, fmt.Errorf("territory availability %q has no territory relationship", territory.ID)
		}
		byCode[territory.Relationships.Territory.Data.ID] = territory
	}
	wanted := make(map[string]AppTerritoryAvailability, len(desired))
	for _, territory := range desired {
		if _, ok := byCode[territory.Territory]; !ok {
			return nil, fmt.Errorf("no existing territory availability for %q", territory.Territory)
		}
		wanted[territory.Territory] = territory
	}
	for _, existing := range current {
		code := existing.Relationships.Territory.Data.ID
		target, ok := wanted[code]
		available := ok
		attrs := models.TerritoryAvailabilityInlineCreateAttributes{}
		if existing.Attributes.Available == nil || *existing.Attributes.Available != available {
			attrs.Available = &available
		}
		if ok {
			if target.ReleaseDate != nil && (existing.Attributes.ReleaseDate == nil || *target.ReleaseDate != *existing.Attributes.ReleaseDate) {
				attrs.ReleaseDate = target.ReleaseDate
			}
			if target.PreOrderEnabled != nil && (existing.Attributes.PreOrderEnabled == nil || *target.PreOrderEnabled != *existing.Attributes.PreOrderEnabled) {
				attrs.PreOrderEnabled = target.PreOrderEnabled
			}
		}
		if attrs.Available == nil && attrs.ReleaseDate == nil && attrs.PreOrderEnabled == nil {
			continue
		}
		// PATCH accepts attributes only, not the create-only territory relationship.
		rb, err := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"type": "territoryAvailabilities", "id": existing.ID, "attributes": attrs}})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest("PATCH", fmt.Sprintf("%s/v1/territoryAvailabilities/%s", c.HostURL, url.PathEscape(existing.ID)), strings.NewReader(string(rb)))
		if err != nil {
			return nil, err
		}
		if _, err := c.doRequest(req, authToken); err != nil {
			return nil, fmt.Errorf("updating territory %s: %w", code, err)
		}
	}
	return availability, nil
}

// CreateAppAvailability sets the storefronts an app is sold in.
//
// POST is only for initial creation. Existing storefronts must be updated
// through their territoryAvailabilities records.
//
// The territory records travel inline under placeholder IDs, the same shape the
// price schedule uses.
func (c *Client) CreateAppAvailability(
	appID string,
	availableInNewTerritories bool,
	territories []AppTerritoryAvailability,
	authToken *string,
) (*models.AppAvailability, error) {
	placeholders := make([]models.ResourceData, 0, len(territories))
	included := make([]models.TerritoryAvailabilityInlineCreate, 0, len(territories))

	for i, territory := range territories {
		placeholderID := fmt.Sprintf("${territory%d}", i)

		placeholders = append(placeholders, models.ResourceData{
			Type: "territoryAvailabilities",
			ID:   placeholderID,
		})

		included = append(included, models.TerritoryAvailabilityInlineCreate{
			Type: "territoryAvailabilities",
			ID:   placeholderID,
			Attributes: &models.TerritoryAvailabilityInlineCreateAttributes{
				Available:       territory.Available,
				ReleaseDate:     territory.ReleaseDate,
				PreOrderEnabled: territory.PreOrderEnabled,
			},
			Relationships: models.TerritoryAvailabilityInlineRelationships{
				Territory: models.ResourceIdentifier{
					Data: models.ResourceData{Type: "territories", ID: territory.Territory},
				},
			},
		})
	}

	requestData := models.AppAvailabilityCreateRequest{
		Data: models.AppAvailabilityCreateData{
			Type: "appAvailabilities",
			Attributes: models.AppAvailabilityCreateAttributes{
				AvailableInNewTerritories: availableInNewTerritories,
			},
			Relationships: models.AppAvailabilityCreateRelationships{
				App: models.ResourceIdentifier{
					Data: models.ResourceData{Type: "apps", ID: appID},
				},
				TerritoryAvailabilities: models.ResourceIdentifiers{Data: placeholders},
			},
		},
		Included: included,
	}

	rb, err := json.Marshal(requestData)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/v2/appAvailabilities", c.HostURL), strings.NewReader(string(rb)))
	if err != nil {
		return nil, err
	}

	body, err := c.doRequest(req, authToken)
	if err != nil {
		return nil, err
	}

	response := models.Response[models.AppAvailability]{}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	return &response.Data, nil
}
