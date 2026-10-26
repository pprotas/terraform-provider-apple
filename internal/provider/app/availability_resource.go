// Copyright (c) AO Studio
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/AhmedOsman00/terraform-provider-apple/internal/apple"
	"github.com/AhmedOsman00/terraform-provider-apple/internal/apple/models"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var (
	_ resource.Resource                = &appAvailabilityResource{}
	_ resource.ResourceWithConfigure   = &appAvailabilityResource{}
	_ resource.ResourceWithImportState = &appAvailabilityResource{}
	_ resource.ResourceWithModifyPlan  = &appAvailabilityResource{}
)

func NewAppAvailabilityResource() resource.Resource {
	return &appAvailabilityResource{}
}

type appAvailabilityResource struct {
	client *apple.Client
}

// Metadata returns the resource type name.
func (r *appAvailabilityResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_app_availability"
}

// Schema defines the schema for the resource.
func (r *appAvailabilityResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the storefronts an app is sold in.\n\n" +
			"This is the other half of App Store Connect's *Pricing and Availability* page; " +
			"`apple_app_price_schedule` is the price. Unlike an in-app purchase's or a subscription's " +
			"availability, which carry a flat list of territory codes, an app's carries a record per " +
			"storefront — each with its own release date and pre-order setting.\n\n" +
			"`available_in_new_territories` covers only storefronts Apple opens **later**, so it is no " +
			"substitute for naming the current ones. To sell everywhere, pass " +
			"`data.apple_territories.all.ids`.\n\n" +
			"Existing storefronts are updated through `PATCH /v1/territoryAvailabilities/{id}`. " +
			"Apple's published API cannot update `available_in_new_territories` on an existing record; " +
			"change that setting in App Store Connect before applying.\n\n" +
			"~> **This resource cannot be destroyed.** There is no way to return an app to having " +
			"no availability at all. `terraform destroy` drops it " +
			"from state and warns. To stop selling in a territory, remove it from `territories` — that " +
			"is a normal in-place update.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The unique Apple-generated identifier for the availability record.",
				Computed:            true,
			},
			"app_id": schema.StringAttribute{
				MarkdownDescription: "The Apple ID of the app. Cannot be changed.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"available_in_new_territories": schema.BoolAttribute{
				MarkdownDescription: "Whether the app is offered automatically in territories the App " +
					"Store adds in future. Defaults to `true`, matching App Store Connect's own " +
					"default. Set it to `false` to keep the territory list exactly as configured.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"territories": schema.SetNestedAttribute{
				MarkdownDescription: "The storefronts the app is sold in. A set, because Apple returns " +
					"them in no particular order. At least one is required.",
				Required: true,
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
				},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"territory": schema.StringAttribute{
							MarkdownDescription: "The three-letter Apple territory code, for example " +
								"`USA`, `GBR` or `EGY` — not the two-letter ISO 3166-1 alpha-2 code.",
							Required:   true,
							Validators: []validator.String{TerritoryValidator},
						},
						"release_date": schema.StringAttribute{
							MarkdownDescription: "The date the app becomes available in this storefront, " +
								"as a plain date in `YYYY-MM-DD` form. Omit it to release as soon as " +
								"the version is approved.",
							Optional:   true,
							Validators: []validator.String{DateValidator},
						},
						"pre_order_enabled": schema.BoolAttribute{
							MarkdownDescription: "Whether customers in this storefront can pre-order the " +
								"app before `release_date`. Requires a release date.",
							Optional: true,
						},
					},
				},
			},
		},
	}
}

// Create a new resource.
func (r *appAvailabilityResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	tflog.Info(ctx, "Creating app availability resource")

	var plan appAvailabilityModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.write(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

// Read refreshes the Terraform state with the latest data.
//
// The territory entries are matched against state by territory code, and the
// ones state already knows about keep their configured dates: Apple fills in a
// release date for a storefront that has already shipped, and adopting it would
// show as a permanent diff against a configuration that never set one -- the
// same trap the price schedule is under.
func (r *appAvailabilityResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	tflog.Debug(ctx, "Reading app availability resource")

	var state appAvailabilityModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	appID := state.AppID.ValueString()
	ctx = tflog.SetField(ctx, "app_id", appID)

	availability, territories, err := r.client.GetAppAvailabilityWithTerritories(appID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "404") {
			tflog.Info(ctx, "App availability not found, removing from state")
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Error Reading App Availability",
			fmt.Sprintf("Could not read the availability of app '%s': %s", appID, err.Error()),
		)
		return
	}

	state.ID = types.StringValue(availability.ID)
	state.AvailableInNewTerritories = boolOrNull(availability.Attributes.AvailableInNewTerritories)
	state.Territories = reconcileTerritories(state.Territories, territories)

	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
}

// Update reconciles the existing storefront records.
func (r *appAvailabilityResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	tflog.Info(ctx, "Updating app availability resource")

	var plan appAvailabilityModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.write(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

// Delete drops the availability from state.
//
// Apple publishes no DELETE, and an app cannot be returned to having no
// availability record.
func (r *appAvailabilityResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state appAvailabilityModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.AddWarning(
		"App Availability Not Deleted",
		fmt.Sprintf("Apple publishes no DELETE endpoint for an app availability, so the storefronts app "+
			"'%s' is sold in remain as they are. Terraform has removed the resource from state "+
			"only.\n\nTo stop selling in a territory, remove it from territories instead of destroying "+
			"this resource.", state.AppID.ValueString()),
	)

	tflog.Info(ctx, "App availability removed from state", map[string]interface{}{
		"app_id": state.AppID.ValueString(),
	})
}

// ModifyPlan leaves the ID unresolved until the availability has been read.
func (r *appAvailabilityResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	if req.Plan.Raw.Equal(req.State.Raw) {
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), types.StringUnknown())...)
}

// Configure adds the provider configured client to the resource.
func (r *appAvailabilityResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*apple.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *apple.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}

	r.client = client
}

// ImportState imports an app's availability.
//
// The import ID is the app ID: an app has exactly one availability record, and
// Apple offers no way to find one without knowing the app.
func (r *appAvailabilityResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	tflog.Info(ctx, "Importing app availability resource", map[string]interface{}{"import_id": req.ID})

	availability, territories, err := r.client.GetAppAvailabilityWithTerritories(req.ID)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Importing App Availability",
			fmt.Sprintf("Could not read the availability of app '%s': %s\n\n"+
				"The import ID is the app's Apple ID — an app has exactly one availability record, and "+
				"Apple publishes no collection of them to look one up in.", req.ID, err.Error()),
		)
		return
	}

	state := appAvailabilityModel{
		ID:                        types.StringValue(availability.ID),
		AppID:                     types.StringValue(req.ID),
		AvailableInNewTerritories: types.BoolValue(false),
		Territories:               reconcileTerritories(nil, territories),
	}
	if availability.Attributes.AvailableInNewTerritories != nil {
		state.AvailableInNewTerritories = types.BoolValue(*availability.Attributes.AvailableInNewTerritories)
	}

	diags := resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
}

// write reconciles availability and records its ID.
func (r *appAvailabilityResource) write(ctx context.Context, plan *appAvailabilityModel, diags *diag.Diagnostics) {
	appID := plan.AppID.ValueString()
	ctx = tflog.SetField(ctx, "app_id", appID)

	// Apple's availability is expressed as "available: true" per storefront
	// rather than as a bare list, so the flag is set explicitly on every entry:
	// a record sent without it is not an omission Apple fills in, it is a
	// storefront with no answer.
	available := true

	territories := make([]apple.AppTerritoryAvailability, 0, len(plan.Territories))
	for _, territory := range plan.Territories {
		territories = append(territories, apple.AppTerritoryAvailability{
			Territory:       territory.Territory.ValueString(),
			Available:       &available,
			ReleaseDate:     stringOrNil(territory.ReleaseDate),
			PreOrderEnabled: boolOrNil(territory.PreOrderEnabled),
		})
	}

	tflog.Debug(ctx, "Writing app availability", map[string]interface{}{
		"territory_count": len(territories),
	})

	availability, err := r.client.SetAppAvailability(
		appID,
		plan.AvailableInNewTerritories.ValueBool(),
		territories,
		nil,
	)
	if err != nil {
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "404"):
			diags.AddError(
				"App or Territory Not Found",
				fmt.Sprintf("Could not write the availability of app '%s': %s\n\n"+
					"Territories are named by three-letter Apple code — USA, GBR, EGY — not by the "+
					"two-letter ISO 3166-1 alpha-2 code.", appID, errMsg),
			)
		case strings.Contains(errMsg, "preOrder"):
			diags.AddError(
				"Pre-Order Rejected",
				fmt.Sprintf("Apple refused the pre-order settings on app '%s': %s\n\n"+
					"A storefront with pre_order_enabled needs a release_date in the future, and "+
					"pre-orders cannot be turned on for an app already on sale there.", appID, errMsg),
			)
		default:
			diags.AddError(
				"Error Writing App Availability",
				fmt.Sprintf("Could not write the availability of app '%s': %s", appID, errMsg),
			)
		}
		tflog.Error(ctx, "Failed to write app availability", map[string]interface{}{"error": errMsg})

		return
	}

	plan.ID = types.StringValue(availability.ID)
}

// reconcileTerritories matches Apple's storefront records against state.
//
// A storefront state already knows about keeps its configured release date and
// pre-order flag, for the reason given on Read. One Apple reports that state
// does not know about is added with Apple's own values, so a change made in App
// Store Connect surfaces as drift rather than vanishing. Storefronts Apple
// reports as not available are dropped: they are the ones the app is not sold
// in, which is the whole point of the list.
func reconcileTerritories(state []appTerritoryAvailabilityModel, reported []models.TerritoryAvailability) []appTerritoryAvailabilityModel {
	byTerritory := make(map[string]models.TerritoryAvailability, len(reported))
	for _, territory := range reported {
		if territory.Attributes.Available != nil && !*territory.Attributes.Available {
			continue
		}
		if territory.Relationships == nil || territory.Relationships.Territory == nil {
			continue
		}
		if code := territory.Relationships.Territory.Data.ID; code != "" {
			byTerritory[code] = territory
		}
	}

	reconciled := make([]appTerritoryAvailabilityModel, 0, len(byTerritory))
	for _, territory := range state {
		code := territory.Territory.ValueString()
		if _, ok := byTerritory[code]; !ok {
			continue
		}
		reconciled = append(reconciled, territory)
		delete(byTerritory, code)
	}

	// Sorted so the additions land in a stable order rather than Go's map
	// iteration order, which would churn the plan between runs.
	added := make([]string, 0, len(byTerritory))
	for code := range byTerritory {
		added = append(added, code)
	}
	sort.Strings(added)

	for _, code := range added {
		territory := byTerritory[code]
		reconciled = append(reconciled, appTerritoryAvailabilityModel{
			Territory:       types.StringValue(code),
			ReleaseDate:     stringOrNull(territory.Attributes.ReleaseDate),
			PreOrderEnabled: boolOrNull(territory.Attributes.PreOrderEnabled),
		})
	}

	return reconciled
}
