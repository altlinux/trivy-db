package alt

import (
	"encoding/json"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/xerrors"

	"github.com/aquasecurity/trivy-db/pkg/db"
	"github.com/aquasecurity/trivy-db/pkg/types"
	ustrings "github.com/aquasecurity/trivy-db/pkg/utils/strings"
	"github.com/aquasecurity/trivy-db/pkg/vulnsrc/vulnerability"
)

const (
	rootBucket = "alt"
)

var (
	vendorCVEs []VendorCVE
	source     = types.DataSource{
		ID:   vulnerability.ALT,
		Name: "alt",
		URL:  "https://rdb.altlinux.org",
	}
)

type VulnSrc struct {
	dbc db.Operation
}

func NewVulnSrc() VulnSrc {
	return VulnSrc{
		dbc: db.Config{},
	}
}

func (vs VulnSrc) Name() types.SourceID {
	return vulnerability.ALT
}

func (vs VulnSrc) Update(dir string) error {
	root := filepath.Join(dir, "vuln-list-alt", "oval")

	branches, err := getBranches(root)
	if err != nil {
		return err
	}

	advisories := map[bucket]AdvisorySpecial{}

	for _, branch := range branches {
		log.Printf("Parsing %s", branch.Name())

		branchDir := filepath.Join(root, branch.Name())

		products, err := getProducts(branchDir)
		if err != nil {
			return err
		}

		for _, f := range products {
			definitions, err := parseOVAL(filepath.Join(branchDir, f.Name()))
			if err != nil {
				return xerrors.Errorf("failed to parse product OVAL: %w", err)
			}

			advisories = vs.mergeAdvisories(advisories, definitions)

		}
	}

	if err = vs.putVendorCVEs(); err != nil {
		return xerrors.Errorf("put vendor cve error: %s", err)
	}

	if err = vs.save(advisories); err != nil {
		return xerrors.Errorf("save error: %w", err)
	}

	return nil
}

func getBranches(dir string) ([]fs.DirEntry, error) {
	branches, err := os.ReadDir(dir)
	if err != nil {
		return nil, xerrors.Errorf("failed to get directory branch list: %w", err)
	}
	return branches, nil
}

func getProducts(dir string) ([]fs.DirEntry, error) {
	products, err := os.ReadDir(dir)
	if err != nil {
		return nil, xerrors.Errorf("failed to get directory products list: %w", err)
	}
	return products, nil
}

func parseOVAL(dir string) (map[bucket]DefinitionSpecial, error) {
	tests, err := resolveTests(dir)
	if err != nil {
		return nil, err
	}

	definitions, err := parseDefinitions(dir)
	if err != nil {
		return nil, err
	}

	defs := map[bucket]DefinitionSpecial{}

	for _, advisory := range definitions.Definition {
		if strings.Contains(advisory.ID, "unaffected") {
			continue
		}

		affectedPkgs := walkCriterion(advisory.Criteria, tests)
		for _, affectedPkg := range affectedPkgs {
			packageName := affectedPkg.Name

			altID := vendorID(advisory.Metadata.References)

			var cveEntries []CVEEntry
			vendorCve := vendorCVE(advisory.Metadata)
			cveEntries = append(cveEntries, vendorCve)

			vendorCVEs = append(vendorCVEs, VendorCVE{CVE: vendorCve, Title: advisory.Metadata.Title,
				Description: advisory.Metadata.Description, References: toReferences(advisory.Metadata.References)})

			for _, bdu := range advisory.Metadata.Advisory.BDUs {
				bduEntry := CVEEntry{
					ID:       bdu.ID,
					Severity: severityFromImpact(bdu.Impact),
				}
				cveEntries = append(cveEntries, bduEntry)
				vendorCVEs = append(vendorCVEs, VendorCVE{CVE: bduEntry, Title: "", Description: "", References: []string{bdu.Href}})
			}

			for _, cve := range advisory.Metadata.Advisory.CVEs {
				cveEntries = append(cveEntries, CVEEntry{
					ID:       cve.ID,
					Severity: severityFromImpact(cve.Impact),
				})
			}

			if altID != "" {
				bkt := bucket{
					packageName:     packageName,
					vulnerabilityID: altID,
				}
				defs[bkt] = DefinitionSpecial{
					Entry: Entry{
						CVEs:            cveEntries,
						FixedVersion:    affectedPkg.FixedVersion,
						AffectedCPEList: advisory.Metadata.Advisory.AffectedCPEs.CPEs,
						Arches:          affectedPkg.Arches,
					},
				}
			} else {
				for _, cve := range cveEntries {
					bkt := bucket{
						packageName:     packageName,
						vulnerabilityID: cve.ID,
					}
					defs[bkt] = DefinitionSpecial{
						Entry: Entry{
							CVEs: []CVEEntry{
								{
									Severity: cve.Severity,
								},
							},
							FixedVersion:    affectedPkg.FixedVersion,
							AffectedCPEList: advisory.Metadata.Advisory.AffectedCPEs.CPEs,
							Arches:          affectedPkg.Arches,
						},
					}
				}
			}
		}
	}

	return defs, nil
}

func walkCriterion(cri Criteria, tests map[string]resolvedTest) []pkg {
	var packages []pkg

	for _, c := range cri.Criterions {
		t, ok := tests[c.TestRef]
		if !ok {
			continue
		}

		var arches []string
		if t.Arch != "" {
			arches = strings.Split(t.Arch, "|")
		}
		packages = append(packages, pkg{
			Name:         t.Name,
			FixedVersion: t.FixedVersion,
			Arches:       arches,
		})
	}

	if len(cri.Criterias) == 0 {
		return packages
	}

	for _, c := range cri.Criterias {
		pkgs := walkCriterion(c, tests)
		if len(pkgs) != 0 {
			packages = append(packages, pkgs...)
		}
	}
	return packages
}

func (vs VulnSrc) mergeAdvisories(advisories map[bucket]AdvisorySpecial, defs map[bucket]DefinitionSpecial) map[bucket]AdvisorySpecial {
	for bkt, def := range defs {
		if old, ok := advisories[bkt]; ok {
			found := false
			for i := range old.Entries {

				if old.Entries[i].FixedVersion == def.Entry.FixedVersion && slices.Equal(old.Entries[i].Arches, def.Entry.Arches) {
					found = true
					old.Entries[i].AffectedCPEList = ustrings.Merge(old.Entries[i].AffectedCPEList, def.Entry.AffectedCPEList)
				}
			}
			if !found {
				old.Entries = append(old.Entries, def.Entry)
			}
			advisories[bkt] = old
		} else {
			advisories[bkt] = AdvisorySpecial{
				Entries: []Entry{def.Entry},
			}
		}
	}

	return advisories
}

func (vs VulnSrc) save(advisories map[bucket]AdvisorySpecial) error {
	err := vs.dbc.BatchUpdate(func(tx *bolt.Tx) error {
		if err := vs.dbc.PutDataSource(tx, rootBucket, source); err != nil {
			return xerrors.Errorf("failed to put data source: %w", err)
		}
		for bkt, advisory := range advisories {
			if err := vs.dbc.PutAdvisoryDetail(tx, bkt.vulnerabilityID, bkt.packageName, []string{rootBucket}, advisory); err != nil {
				return xerrors.Errorf("failed to save ALT OVAL advisory: %w", err)
			}

			if err := vs.dbc.PutVulnerabilityID(tx, bkt.vulnerabilityID); err != nil {
				return xerrors.Errorf("failed to put severity: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return xerrors.Errorf("batch update error: %w", err)
	}
	return nil
}

func vendorCVE(metadata Metadata) CVEEntry {
	var id string
	for _, r := range metadata.References {
		if strings.Contains(r.RefID, "ALT") {
			id = r.RefID
		}
	}
	return CVEEntry{ID: id, Severity: severityFromImpact(metadata.Advisory.Severity)}
}

func (vs VulnSrc) putVendorCVEs() error {
	err := vs.dbc.BatchUpdate(func(tx *bolt.Tx) error {
		for _, cve := range vendorCVEs {
			err := vs.putVendorVulnerabilityDetail(tx, cve)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func (vs VulnSrc) putVendorVulnerabilityDetail(tx *bolt.Tx, cve VendorCVE) error {
	vuln := types.VulnerabilityDetail{
		CvssScore:    0,
		CvssVector:   "",
		CvssScoreV3:  0,
		CvssVectorV3: "",
		Severity:     cve.CVE.Severity,
		References:   cve.References,
		Title:        cve.Title,
		Description:  cve.Description,
	}

	if err := vs.dbc.PutVulnerabilityDetail(tx, cve.CVE.ID, vulnerability.ALT, vuln); err != nil {
		return xerrors.Errorf("failed to save ALT Vendor vulnerability: %w", err)
	}

	if err := vs.dbc.PutVulnerabilityID(tx, cve.CVE.ID); err != nil {
		return xerrors.Errorf("failed to save the vulnerability ID: %w", err)
	}
	return nil
}

func (vs VulnSrc) Get(pkgName, cpe string) ([]types.Advisory, error) {
	rawAdvisories, err := vs.dbc.ForEachAdvisory([]string{rootBucket}, pkgName)
	if err != nil {
		return nil, xerrors.Errorf("unable to iterate advisories: %w", err)
	}

	var advisories []types.Advisory
	for vulnID, v := range rawAdvisories {
		if len(v.Content) == 0 {
			continue
		}

		var adv AdvisorySpecial
		if err = json.Unmarshal(v.Content, &adv); err != nil {
			return nil, xerrors.Errorf("failed to unmarshal advisory JSON: %w", err)
		}

		for _, entry := range adv.Entries {
			if !contains(entry.AffectedCPEList, cpe) {
				continue
			}
			for _, cve := range entry.CVEs {
				advisory := types.Advisory{
					Severity:     cve.Severity,
					FixedVersion: entry.FixedVersion,
					Arches:       entry.Arches,
				}

				if strings.HasPrefix(vulnID, "CVE-") {
					advisory.VulnerabilityID = vulnID
				} else {
					advisory.VulnerabilityID = cve.ID
					advisory.VendorIDs = []string{vulnID}
				}

				advisories = append(advisories, advisory)
			}
		}
	}

	return advisories, nil
}
func contains(lst []string, val string) bool {
	for _, e := range lst {
		if e == val {
			return true
		}
	}
	return false
}

func toReferences(references []Reference) []string {
	var data []string
	for _, r := range references {
		data = append(data, r.RefURL)
	}
	return data
}

func severityFromImpact(sev string) types.Severity {
	switch strings.ToLower(sev) {
	case "low":
		return types.SeverityLow
	case "medium":
		return types.SeverityMedium
	case "high":
		return types.SeverityHigh
	case "critical":
		return types.SeverityCritical
	}
	return types.SeverityUnknown
}

func vendorID(refs []Reference) string {
	for _, ref := range refs {
		switch ref.Source {
		case "ALTPU":
			return ref.RefID
		}
	}
	return ""
}
