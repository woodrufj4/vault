package pki

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/vault/sdk/helper/consts"

	"github.com/hashicorp/vault/sdk/helper/certutil"
	"github.com/hashicorp/vault/sdk/helper/errutil"
	"github.com/hashicorp/vault/sdk/logical"
)

const revokedPath = "revoked/"

type revocationInfo struct {
	CertificateBytes  []byte    `json:"certificate_bytes"`
	RevocationTime    int64     `json:"revocation_time"`
	RevocationTimeUTC time.Time `json:"revocation_time_utc"`
	CertificateIssuer issuerID  `json:"issuer_id"`
}

// crlBuilder is gatekeeper for controlling various read/write operations to the storage of the CRL.
// The extra complexity arises from secondary performance clusters seeing various writes to its storage
// without the actual API calls. During the storage invalidation process, we do not have the required state
// to actually rebuild the CRLs, so we need to schedule it in a deferred fashion. This allows either
// read or write calls to perform the operation if required, or have the flag reset upon a write operation
type crlBuilder struct {
	m            sync.Mutex
	forceRebuild uint32
}

const (
	_ignoreForceFlag  = true
	_enforceForceFlag = false
)

// rebuildIfForced is to be called by readers or periodic functions that might need to trigger
// a refresh of the CRL before the read occurs.
func (cb *crlBuilder) rebuildIfForced(ctx context.Context, b *backend, request *logical.Request) error {
	if atomic.LoadUint32(&cb.forceRebuild) == 1 {
		return cb._doRebuild(ctx, b, request, true, _enforceForceFlag)
	}

	return nil
}

// rebuild is to be called by various write apis that know the CRL is to be updated and can be now.
func (cb *crlBuilder) rebuild(ctx context.Context, b *backend, request *logical.Request, forceNew bool) error {
	return cb._doRebuild(ctx, b, request, forceNew, _ignoreForceFlag)
}

// requestRebuildIfActiveNode will schedule a rebuild of the CRL from the next read or write api call assuming we are the active node of a cluster
func (cb *crlBuilder) requestRebuildIfActiveNode(b *backend) {
	// Only schedule us on active nodes, ignoring secondary nodes, the active can/should rebuild the CRL.
	if b.System().ReplicationState().HasState(consts.ReplicationPerformanceStandby) ||
		b.System().ReplicationState().HasState(consts.ReplicationDRSecondary) {
		b.Logger().Debug("Ignoring request to schedule a CRL rebuild, not on active node.")
		return
	}

	b.Logger().Info("Scheduling PKI CRL rebuild.")
	cb.m.Lock()
	defer cb.m.Unlock()
	atomic.StoreUint32(&cb.forceRebuild, 1)
}

func (cb *crlBuilder) _doRebuild(ctx context.Context, b *backend, request *logical.Request, forceNew bool, ignoreForceFlag bool) error {
	cb.m.Lock()
	defer cb.m.Unlock()
	if cb.forceRebuild == 1 || ignoreForceFlag {
		defer atomic.StoreUint32(&cb.forceRebuild, 0)

		// if forceRebuild was requested, that should force a complete rebuild even if requested not too by forceNew
		myForceNew := cb.forceRebuild == 1 || forceNew
		return buildCRLs(ctx, b, request, myForceNew)
	}

	return nil
}

// Revokes a cert, and tries to be smart about error recovery
func revokeCert(ctx context.Context, b *backend, req *logical.Request, serial string, fromLease bool) (*logical.Response, error) {
	// As this backend is self-contained and this function does not hook into
	// third parties to manage users or resources, if the mount is tainted,
	// revocation doesn't matter anyways -- the CRL that would be written will
	// be immediately blown away by the view being cleared. So we can simply
	// fast path a successful exit.
	if b.System().Tainted() {
		return nil, nil
	}

	signingBundle, caErr := fetchCAInfo(ctx, b, req, defaultRef)
	if caErr != nil {
		switch caErr.(type) {
		case errutil.UserError:
			return logical.ErrorResponse(fmt.Sprintf("could not fetch the CA certificate: %s", caErr)), nil
		default:
			return nil, fmt.Errorf("error fetching CA certificate: %s", caErr)
		}
	}

	if signingBundle == nil {
		return nil, errors.New("CA info not found")
	}
	colonSerial := strings.Replace(strings.ToLower(serial), "-", ":", -1)
	if colonSerial == certutil.GetHexFormatted(signingBundle.Certificate.SerialNumber.Bytes(), ":") {
		return logical.ErrorResponse("adding CA to CRL is not allowed"), nil
	}

	alreadyRevoked := false
	var revInfo revocationInfo

	revEntry, err := fetchCertBySerial(ctx, b, req, revokedPath, serial)
	if err != nil {
		switch err.(type) {
		case errutil.UserError:
			return logical.ErrorResponse(err.Error()), nil
		default:
			return nil, err
		}
	}
	if revEntry != nil {
		// Set the revocation info to the existing values
		alreadyRevoked = true
		err = revEntry.DecodeJSON(&revInfo)
		if err != nil {
			return nil, fmt.Errorf("error decoding existing revocation info")
		}
	}

	if !alreadyRevoked {
		certEntry, err := fetchCertBySerial(ctx, b, req, "certs/", serial)
		if err != nil {
			switch err.(type) {
			case errutil.UserError:
				return logical.ErrorResponse(err.Error()), nil
			default:
				return nil, err
			}
		}
		if certEntry == nil {
			if fromLease {
				// We can't write to revoked/ or update the CRL anyway because we don't have the cert,
				// and there's no reason to expect this will work on a subsequent
				// retry.  Just give up and let the lease get deleted.
				b.Logger().Warn("expired certificate revoke failed because not found in storage, treating as success", "serial", serial)
				return nil, nil
			}
			return logical.ErrorResponse(fmt.Sprintf("certificate with serial %s not found", serial)), nil
		}

		cert, err := x509.ParseCertificate(certEntry.Value)
		if err != nil {
			return nil, fmt.Errorf("error parsing certificate: %w", err)
		}
		if cert == nil {
			return nil, fmt.Errorf("got a nil certificate")
		}

		// Add a little wiggle room because leases are stored with a second
		// granularity
		if cert.NotAfter.Before(time.Now().Add(2 * time.Second)) {
			return nil, nil
		}

		// Compatibility: Don't revoke CAs if they had leases. New CAs going
		// forward aren't issued leases.
		if cert.IsCA && fromLease {
			return nil, nil
		}

		currTime := time.Now()
		revInfo.CertificateBytes = certEntry.Value
		revInfo.RevocationTime = currTime.Unix()
		revInfo.RevocationTimeUTC = currTime.UTC()

		revEntry, err = logical.StorageEntryJSON(revokedPath+normalizeSerial(serial), revInfo)
		if err != nil {
			return nil, fmt.Errorf("error creating revocation entry")
		}

		err = req.Storage.Put(ctx, revEntry)
		if err != nil {
			return nil, fmt.Errorf("error saving revoked certificate to new location")
		}
	}

	crlErr := b.crlBuilder.rebuild(ctx, b, req, false)
	if crlErr != nil {
		switch crlErr.(type) {
		case errutil.UserError:
			return logical.ErrorResponse(fmt.Sprintf("Error during CRL building: %s", crlErr)), nil
		default:
			return nil, fmt.Errorf("error encountered during CRL building: %w", crlErr)
		}
	}

	resp := &logical.Response{
		Data: map[string]interface{}{
			"revocation_time": revInfo.RevocationTime,
		},
	}
	if !revInfo.RevocationTimeUTC.IsZero() {
		resp.Data["revocation_time_rfc3339"] = revInfo.RevocationTimeUTC.Format(time.RFC3339Nano)
	}
	return resp, nil
}

func buildCRLs(ctx context.Context, b *backend, req *logical.Request, forceNew bool) error {
	// In order to build all CRLs, we need knowledge of all issuers. Any two
	// issuers with the same keys _and_ subject should have the same CRL since
	// they're functionally equivalent.
	//
	// When building CRLs, there's two types of CRLs: an "internal" CRL for
	// just certificates issued by this issuer, and a "default" CRL, which
	// not only contains certificates by this issuer, but also ones issued
	// by "unknown" or past issuers. This means we need knowledge of not
	// only all issuers (to tell whether or not to include these orphaned
	// certs) but whether the present issuer is the configured default.
	//
	// If a configured default is lacking, we won't provision these
	// certificates on any CRL.
	//
	// In order to know which CRL a given cert belongs on, we have to read
	// it into memory, identify the corresponding issuer, and update its
	// map with the revoked cert instance. If no such issuer is found, we'll
	// place it in the default issuer's CRL.
	//
	// By not updating storage, we allow issuers to come and go (either by
	// direct deletion or by having their keys delete, preventing CRLs from
	// being signed) -- and when they return, we'll correctly place certs
	// on their CRLs.
	issuers, err := listIssuers(ctx, req.Storage)
	if err != nil {
		return fmt.Errorf("error building CRL: while listing issuers: %v", err)
	}

	config, err := getIssuersConfig(ctx, req.Storage)
	if err != nil {
		return fmt.Errorf("error building CRLs: while getting the default config: %v", err)
	}

	// We map issuerID->entry for fast lookup and also issuerID->Cert for
	// signature verification and correlation of revoked certs.
	issuerIDEntryMap := make(map[issuerID]*issuerEntry, len(issuers))
	issuerIDCertMap := make(map[issuerID]*x509.Certificate, len(issuers))

	// We use a double map (keyID->subject->issuerID) to store whether or not this
	// key+subject paring has been seen before. We can then iterate over each
	// key/subject and choose any representative issuer for that combination.
	keySubjectIssuersMap := make(map[keyID]map[string][]issuerID)
	for _, issuer := range issuers {
		thisEntry, err := fetchIssuerById(ctx, req.Storage, issuer)
		if err != nil {
			return fmt.Errorf("error building CRLs: unable to fetch specified issuer (%v): %v", issuer, err)
		}

		if len(thisEntry.KeyID) == 0 {
			continue
		}

		issuerIDEntryMap[issuer] = thisEntry

		thisCert, err := thisEntry.GetCertificate()
		if err != nil {
			return fmt.Errorf("error building CRLs: unable to parse issuer (%v)'s certificate: %v", issuer, err)
		}
		issuerIDCertMap[issuer] = thisCert

		subject := string(thisCert.RawIssuer)
		if _, ok := keySubjectIssuersMap[thisEntry.KeyID]; !ok {
			keySubjectIssuersMap[thisEntry.KeyID] = make(map[string][]issuerID)
		}

		keySubjectIssuersMap[thisEntry.KeyID][subject] = append(keySubjectIssuersMap[thisEntry.KeyID][subject], issuer)
	}

	// Fetch the cluster-local CRL mapping so we know where to write the
	// CRLs.
	crlConfig, err := getLocalCRLConfig(ctx, req.Storage)
	if err != nil {
		return fmt.Errorf("error building CRLs: unable to fetch cluster-local CRL configuration: %v", err)
	}

	// Next, we load and parse all revoked certificates. We need to assign
	// these certificates to an issuer. Some certificates will not be
	// assignable (if they were issued by a since-deleted issuer), so we need
	// a separate pool for those.
	unassignedCerts, revokedCertsMap, err := getRevokedCertEntries(ctx, req, issuerIDCertMap)
	if err != nil {
		return fmt.Errorf("error building CRLs: unable to get revoked certificate entries: %v", err)
	}

	// Now we can call buildCRL once, on an arbitrary/representative issuer
	// from each of these (keyID, subject) sets.
	for _, subjectIssuersMap := range keySubjectIssuersMap {
		for _, issuersSet := range subjectIssuersMap {
			if len(issuersSet) == 0 {
				continue
			}

			var revokedCerts []pkix.RevokedCertificate
			representative := issuersSet[0]
			var crlIdentifier crlID
			var crlIdIssuer issuerID
			for _, issuerId := range issuersSet {
				if issuerId == config.DefaultIssuerId {
					if len(unassignedCerts) > 0 {
						revokedCerts = append(revokedCerts, unassignedCerts...)
					}

					representative = issuerId
				}

				if thisRevoked, ok := revokedCertsMap[issuerId]; ok && len(thisRevoked) > 0 {
					revokedCerts = append(revokedCerts, thisRevoked...)
				}

				if thisCRLId, ok := crlConfig.IssuerIDCRLMap[issuerId]; ok && len(thisCRLId) > 0 {
					if len(crlIdentifier) > 0 && crlIdentifier != thisCRLId {
						return fmt.Errorf("error building CRLs: two issuers with same keys/subjects (%v vs %v) have different internal CRL IDs: %v vs %v", issuerId, crlIdIssuer, thisCRLId, crlIdentifier)
					}

					crlIdentifier = thisCRLId
					crlIdIssuer = issuerId
				}
			}

			if len(crlIdentifier) == 0 {
				// Create a new random UUID for this CRL if none exists.
				crlIdentifier = genCRLId()
				crlConfig.CRLNumberMap[crlIdentifier] = 1
			}

			// Update all issuers in this group to set the CRL Issuer
			for _, issuerId := range issuersSet {
				crlConfig.IssuerIDCRLMap[issuerId] = crlIdentifier
			}

			// We always update the CRL Number since we never want to
			// duplicate numbers and missing numbers is fine.
			crlNumber := crlConfig.CRLNumberMap[crlIdentifier]
			crlConfig.CRLNumberMap[crlIdentifier] += 1

			// Lastly, build the CRL.
			if err := buildCRL(ctx, b, req, forceNew, representative, revokedCerts, crlIdentifier, crlNumber); err != nil {
				return fmt.Errorf("error building CRLs: unable to build CRL for issuer (%v): %v", representative, err)
			}
		}
	}

	// Finally, persist our potentially updated local CRL config
	if err := setLocalCRLConfig(ctx, req.Storage, crlConfig); err != nil {
		return fmt.Errorf("error building CRLs: unable to persist updated cluster-local CRL config: %v", err)
	}

	// All good :-)
	return nil
}

func getRevokedCertEntries(ctx context.Context, req *logical.Request, issuerIDCertMap map[issuerID]*x509.Certificate) ([]pkix.RevokedCertificate, map[issuerID][]pkix.RevokedCertificate, error) {
	var unassignedCerts []pkix.RevokedCertificate
	revokedCertsMap := make(map[issuerID][]pkix.RevokedCertificate)

	revokedSerials, err := req.Storage.List(ctx, revokedPath)
	if err != nil {
		return nil, nil, errutil.InternalError{Err: fmt.Sprintf("error fetching list of revoked certs: %s", err)}
	}

	for _, serial := range revokedSerials {
		var revInfo revocationInfo
		revokedEntry, err := req.Storage.Get(ctx, revokedPath+serial)
		if err != nil {
			return nil, nil, errutil.InternalError{Err: fmt.Sprintf("unable to fetch revoked cert with serial %s: %s", serial, err)}
		}
		if revokedEntry == nil {
			return nil, nil, errutil.InternalError{Err: fmt.Sprintf("revoked certificate entry for serial %s is nil", serial)}
		}
		if revokedEntry.Value == nil || len(revokedEntry.Value) == 0 {
			// TODO: In this case, remove it and continue? How likely is this to
			// happen? Alternately, could skip it entirely, or could implement a
			// delete function so that there is a way to remove these
			return nil, nil, errutil.InternalError{Err: fmt.Sprintf("found revoked serial but actual certificate is empty")}
		}

		err = revokedEntry.DecodeJSON(&revInfo)
		if err != nil {
			return nil, nil, errutil.InternalError{Err: fmt.Sprintf("error decoding revocation entry for serial %s: %s", serial, err)}
		}

		revokedCert, err := x509.ParseCertificate(revInfo.CertificateBytes)
		if err != nil {
			return nil, nil, errutil.InternalError{Err: fmt.Sprintf("unable to parse stored revoked certificate with serial %s: %s", serial, err)}
		}

		// NOTE: We have to change this to UTC time because the CRL standard
		// mandates it but Go will happily encode the CRL without this.
		newRevCert := pkix.RevokedCertificate{
			SerialNumber: revokedCert.SerialNumber,
		}
		if !revInfo.RevocationTimeUTC.IsZero() {
			newRevCert.RevocationTime = revInfo.RevocationTimeUTC
		} else {
			newRevCert.RevocationTime = time.Unix(revInfo.RevocationTime, 0).UTC()
		}

		// If we have a CertificateIssuer field on the revocation entry,
		// prefer it to manually checking each issuer signature, assuming it
		// appears valid. Its highly unlikely for two different issuers
		// to have the same id (after the first was deleted).
		if len(revInfo.CertificateIssuer) > 0 {
			issuerId := revInfo.CertificateIssuer
			if _, issuerExists := issuerIDCertMap[issuerId]; issuerExists {
				revokedCertsMap[issuerId] = append(revokedCertsMap[issuerId], newRevCert)
				continue
			}

			// Otherwise, fall through and update the entry.
		}

		// Now we need to assign the revoked certificate to an issuer.
		foundParent := false
		for issuerId, issuerCert := range issuerIDCertMap {
			if bytes.Equal(revokedCert.RawIssuer, issuerCert.RawSubject) {
				if err := revokedCert.CheckSignatureFrom(issuerCert); err == nil {
					// Valid mapping. Add it to the specified entry.
					revokedCertsMap[issuerId] = append(revokedCertsMap[issuerId], newRevCert)
					revInfo.CertificateIssuer = issuerId
					foundParent = true
					break
				}
			}
		}

		if !foundParent {
			// If the parent isn't found, add it to the unassigned bucket.
			unassignedCerts = append(unassignedCerts, newRevCert)
		} else {
			// When the CertificateIssuer field wasn't found on the existing
			// entry (or was invalid), and we've found a new value for it,
			// we should update the entry to make future CRL builds faster.
			revokedEntry, err = logical.StorageEntryJSON(revokedPath+serial, revInfo)
			if err != nil {
				return nil, nil, fmt.Errorf("error creating revocation entry for existing cert: %v", serial)
			}

			err = req.Storage.Put(ctx, revokedEntry)
			if err != nil {
				return nil, nil, fmt.Errorf("error updating revoked certificate at existing location: %v", serial)
			}
		}
	}

	return unassignedCerts, revokedCertsMap, nil
}

// Builds a CRL by going through the list of revoked certificates and building
// a new CRL with the stored revocation times and serial numbers.
func buildCRL(ctx context.Context, b *backend, req *logical.Request, forceNew bool, thisIssuerId issuerID, revoked []pkix.RevokedCertificate, identifier crlID, crlNumber int64) error {
	crlInfo, err := b.CRL(ctx, req.Storage)
	if err != nil {
		return errutil.InternalError{Err: fmt.Sprintf("error fetching CRL config information: %s", err)}
	}

	crlLifetime := b.crlLifetime
	var revokedCerts []pkix.RevokedCertificate

	if crlInfo != nil {
		if crlInfo.Expiry != "" {
			crlDur, err := time.ParseDuration(crlInfo.Expiry)
			if err != nil {
				return errutil.InternalError{Err: fmt.Sprintf("error parsing CRL duration of %s", crlInfo.Expiry)}
			}
			crlLifetime = crlDur
		}

		if crlInfo.Disable {
			if !forceNew {
				return nil
			}

			// NOTE: in this case, the passed argument (revoked) is not added
			// to the revokedCerts list. This is because we want to sign an
			// **empty** CRL (as the CRL was disabled but we've specified the
			// forceNew option). In previous versions of Vault (1.10 series and
			// earlier), we'd have queried the certs below, whereas we now have
			// an assignment from a pre-queried list.
			goto WRITE
		}
	}

	revokedCerts = revoked

WRITE:
	_, bundle, caErr := fetchCertBundleByIssuerId(ctx, req.Storage, thisIssuerId, true /* need the signing key */)
	if caErr != nil {
		switch caErr.(type) {
		case errutil.UserError:
			return errutil.UserError{Err: fmt.Sprintf("could not fetch the CA certificate: %s", caErr)}
		default:
			return errutil.InternalError{Err: fmt.Sprintf("error fetching CA certificate: %s", caErr)}
		}
	}

	signingBundle, caErr := parseCABundle(ctx, b, req, bundle)
	if caErr != nil {
		switch caErr.(type) {
		case errutil.UserError:
			return errutil.UserError{Err: fmt.Sprintf("could not fetch the CA certificate: %s", caErr)}
		default:
			return errutil.InternalError{Err: fmt.Sprintf("error fetching CA certificate: %s", caErr)}
		}
	}

	revocationListTemplate := &x509.RevocationList{
		RevokedCertificates: revokedCerts,
		Number:              big.NewInt(crlNumber),
		ThisUpdate:          time.Now(),
		NextUpdate:          time.Now().Add(crlLifetime),
	}

	crlBytes, err := x509.CreateRevocationList(rand.Reader, revocationListTemplate, signingBundle.Certificate, signingBundle.PrivateKey)
	if err != nil {
		return errutil.InternalError{Err: fmt.Sprintf("error creating new CRL: %s", err)}
	}

	err = req.Storage.Put(ctx, &logical.StorageEntry{
		Key:   "crls/" + identifier.String(),
		Value: crlBytes,
	})
	if err != nil {
		return errutil.InternalError{Err: fmt.Sprintf("error storing CRL: %s", err)}
	}

	return nil
}
