package pki

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/sdk/helper/certutil"
	"github.com/hashicorp/vault/sdk/helper/errutil"
	"github.com/hashicorp/vault/sdk/logical"
)

const (
	storageKeyConfig      = "config/keys"
	storageIssuerConfig   = "config/issuers"
	keyPrefix             = "config/key/"
	issuerPrefix          = "config/issuer/"
	storageLocalCRLConfig = "crls/config"

	legacyMigrationBundleLogKey = "config/legacyMigrationBundleLog"
	legacyCertBundlePath        = "config/ca_bundle"
)

type keyID string

func (p keyID) String() string {
	return string(p)
}

type issuerID string

func (p issuerID) String() string {
	return string(p)
}

type crlID string

func (p crlID) String() string {
	return string(p)
}

const (
	IssuerRefNotFound = issuerID("not-found")
	KeyRefNotFound    = keyID("not-found")
)

type keyEntry struct {
	ID             keyID                   `json:"id" structs:"id" mapstructure:"id"`
	Name           string                  `json:"name" structs:"name" mapstructure:"name"`
	PrivateKeyType certutil.PrivateKeyType `json:"private_key_type" structs:"private_key_type" mapstructure:"private_key_type"`
	PrivateKey     string                  `json:"private_key" structs:"private_key" mapstructure:"private_key"`
}

type issuerEntry struct {
	ID                   issuerID                  `json:"id" structs:"id" mapstructure:"id"`
	Name                 string                    `json:"name" structs:"name" mapstructure:"name"`
	KeyID                keyID                     `json:"key_id" structs:"key_id" mapstructure:"key_id"`
	Certificate          string                    `json:"certificate" structs:"certificate" mapstructure:"certificate"`
	CAChain              []string                  `json:"ca_chain" structs:"ca_chain" mapstructure:"ca_chain"`
	ManualChain          []issuerID                `json:"manual_chain" structs:"manual_chain" mapstructure:"manual_chain"`
	SerialNumber         string                    `json:"serial_number" structs:"serial_number" mapstructure:"serial_number"`
	LeafNotAfterBehavior certutil.NotAfterBehavior `json:"not_after_behavior" structs:"not_after_behavior" mapstructure:"not_after_behavior"`
}

type localCRLConfigEntry struct {
	IssuerIDCRLMap map[issuerID]crlID `json:"issuer_id_crl_map" structs:"issuer_id_crl_map" mapstructure:"issuer_id_crl_map"`
	CRLNumberMap   map[crlID]int64    `json:"crl_number_map" structs:"crl_number_map" mapstructure:"crl_number_map"`
}

type keyConfigEntry struct {
	DefaultKeyId keyID `json:"default" structs:"default" mapstructure:"default"`
}

type issuerConfigEntry struct {
	DefaultIssuerId issuerID `json:"default" structs:"default" mapstructure:"default"`
}

func (k keyEntry) GetSigner() (crypto.Signer, error) {
	signer, _, err := certutil.ParsePEMKey(k.PrivateKey)
	return signer, err
}

func listKeys(ctx context.Context, s logical.Storage) ([]keyID, error) {
	strList, err := s.List(ctx, keyPrefix)
	if err != nil {
		return nil, err
	}

	keyIds := make([]keyID, 0, len(strList))
	for _, entry := range strList {
		keyIds = append(keyIds, keyID(entry))
	}

	return keyIds, nil
}

func fetchKeyById(ctx context.Context, s logical.Storage, keyId keyID) (*keyEntry, error) {
	if len(keyId) == 0 {
		return nil, errutil.InternalError{Err: fmt.Sprintf("unable to fetch pki key: empty key identifier")}
	}

	entry, err := s.Get(ctx, keyPrefix+keyId.String())
	if err != nil {
		return nil, errutil.InternalError{Err: fmt.Sprintf("unable to fetch pki key: %v", err)}
	}
	if entry == nil {
		// FIXME: Dedicated/specific error for this?
		return nil, errutil.UserError{Err: fmt.Sprintf("pki key id %s does not exist", keyId.String())}
	}

	var key keyEntry
	if err := entry.DecodeJSON(&key); err != nil {
		return nil, errutil.InternalError{Err: fmt.Sprintf("unable to decode pki key with id %s: %v", keyId.String(), err)}
	}

	return &key, nil
}

func writeKey(ctx context.Context, s logical.Storage, key keyEntry) error {
	keyId := key.ID

	json, err := logical.StorageEntryJSON(keyPrefix+keyId.String(), key)
	if err != nil {
		return err
	}

	return s.Put(ctx, json)
}

func deleteKey(ctx context.Context, s logical.Storage, id keyID) (bool, error) {
	wasDefault := false

	config, err := getKeysConfig(ctx, s)
	if err != nil {
		return wasDefault, err
	}

	if config.DefaultKeyId == id {
		wasDefault = true
		config.DefaultKeyId = keyID("")
		if err := setKeysConfig(ctx, s, config); err != nil {
			return wasDefault, err
		}
	}

	return wasDefault, s.Delete(ctx, keyPrefix+id.String())
}

func importKey(ctx context.Context, s logical.Storage, keyValue string, keyName string) (*keyEntry, bool, error) {
	// importKey imports the specified PEM-format key (from keyValue) into
	// the new PKI storage format. The first return field is a reference to
	// the new key; the second is whether or not the key already existed
	// during import (in which case, *key points to the existing key reference
	// and identifier); the last return field is whether or not an error
	// occurred.
	//
	// Normalize whitespace before beginning.  See note in importIssuer as to
	// why we do this.
	keyValue = strings.TrimSpace(keyValue) + "\n"
	//
	// Before we can import a known key, we first need to know if the key
	// exists in storage already. This means iterating through all known
	// keys and comparing their private value against this value.
	knownKeys, err := listKeys(ctx, s)
	if err != nil {
		return nil, false, err
	}

	// Before we return below, we need to iterate over _all_ issuers and see if
	// one of them has a missing KeyId link, and if so, point it back to
	// ourselves. We fetch the list of issuers up front, even when don't need
	// it, to give ourselves a better chance of succeeding below.
	knownIssuers, err := listIssuers(ctx, s)
	if err != nil {
		return nil, false, err
	}

	for _, identifier := range knownKeys {
		existingKey, err := fetchKeyById(ctx, s, identifier)
		if err != nil {
			return nil, false, err
		}

		if existingKey.PrivateKey == keyValue {
			// Here, we don't need to stitch together the issuer entries,
			// because the last run should've done that for us (or, when
			// importing an issuer).
			return existingKey, true, nil
		}
	}

	// Haven't found a key, so we've gotta create it and write it into storage.
	var result keyEntry
	result.ID = genKeyId()
	result.Name = keyName
	result.PrivateKey = keyValue

	// Extracting the signer is necessary for two reasons: first, to get the
	// public key for comparison with existing issuers; second, to get the
	// corresponding private key type.
	keySigner, err := result.GetSigner()
	if err != nil {
		return nil, false, err
	}
	keyPublic := keySigner.Public()
	result.PrivateKeyType = certutil.GetPrivateKeyTypeFromSigner(keySigner)

	// Finally, we can write the key to storage.
	if err := writeKey(ctx, s, result); err != nil {
		return nil, false, err
	}

	// Now, for each issuer, try and compute the issuer<->key link if missing.
	for _, identifier := range knownIssuers {
		existingIssuer, err := fetchIssuerById(ctx, s, identifier)
		if err != nil {
			return nil, false, err
		}

		// If the KeyID value is already present, we can skip it.
		if len(existingIssuer.KeyID) > 0 {
			continue
		}

		// Otherwise, compare public values. Note that there might be multiple
		// certificates (e.g., cross-signed) with the same key.

		cert, err := existingIssuer.GetCertificate()
		if err != nil {
			// Malformed issuer.
			return nil, false, err
		}

		equal, err := certutil.ComparePublicKeysAndType(cert.PublicKey, keyPublic)
		if err != nil {
			return nil, false, err
		}

		if equal {
			// These public keys are equal, so this key entry must be the
			// corresponding private key to this issuer; update it accordingly.
			existingIssuer.KeyID = result.ID
			if err := writeIssuer(ctx, s, existingIssuer); err != nil {
				return nil, false, err
			}
		}
	}

	// If there was no prior default value set and/or we had no known
	// keys when we started, set this key as default.
	keyDefaultSet, err := isDefaultKeySet(ctx, s)
	if err != nil {
		return nil, false, err
	}
	if len(knownKeys) == 0 || !keyDefaultSet {
		if err = updateDefaultKeyId(ctx, s, result.ID); err != nil {
			return nil, false, err
		}
	}

	// All done; return our new key reference.
	return &result, false, nil
}

func (i issuerEntry) GetCertificate() (*x509.Certificate, error) {
	block, extra := pem.Decode([]byte(i.Certificate))
	if block == nil {
		return nil, errutil.InternalError{Err: fmt.Sprintf("unable to parse certificate from issuer: invalid PEM: %v", i.ID)}
	}
	if len(strings.TrimSpace(string(extra))) > 0 {
		return nil, errutil.InternalError{Err: fmt.Sprintf("unable to parse certificate for issuer (%v): trailing PEM data: %v", i.ID, string(extra))}
	}

	return x509.ParseCertificate(block.Bytes)
}

func listIssuers(ctx context.Context, s logical.Storage) ([]issuerID, error) {
	strList, err := s.List(ctx, issuerPrefix)
	if err != nil {
		return nil, err
	}

	issuerIds := make([]issuerID, 0, len(strList))
	for _, entry := range strList {
		issuerIds = append(issuerIds, issuerID(entry))
	}

	return issuerIds, nil
}

func resolveKeyReference(ctx context.Context, s logical.Storage, reference string) (keyID, error) {
	if reference == defaultRef {
		// Handle fetching the default key.
		config, err := getKeysConfig(ctx, s)
		if err != nil {
			return keyID("config-error"), err
		}
		if len(config.DefaultKeyId) == 0 {
			return KeyRefNotFound, fmt.Errorf("no default key currently configured")
		}

		return config.DefaultKeyId, nil
	}

	keys, err := listKeys(ctx, s)
	if err != nil {
		return keyID("list-error"), err
	}

	// Cheaper to list keys and check if an id is a match...
	for _, keyId := range keys {
		if keyId == keyID(reference) {
			return keyId, nil
		}
	}

	// ... than to pull all keys from storage.
	for _, keyId := range keys {
		key, err := fetchKeyById(ctx, s, keyId)
		if err != nil {
			return keyID("key-read"), err
		}

		if key.Name == reference {
			return key.ID, nil
		}
	}

	// Otherwise, we must not have found the key.
	return KeyRefNotFound, errutil.UserError{Err: fmt.Sprintf("unable to find PKI key for reference: %v", reference)}
}

func fetchIssuerById(ctx context.Context, s logical.Storage, issuerId issuerID) (*issuerEntry, error) {
	if len(issuerId) == 0 {
		return nil, errutil.InternalError{Err: fmt.Sprintf("unable to fetch pki issuer: empty issuer identifier")}
	}

	entry, err := s.Get(ctx, issuerPrefix+issuerId.String())
	if err != nil {
		return nil, errutil.InternalError{Err: fmt.Sprintf("unable to fetch pki issuer: %v", err)}
	}
	if entry == nil {
		// FIXME: Dedicated/specific error for this?
		return nil, errutil.UserError{Err: fmt.Sprintf("pki issuer id %s does not exist", issuerId.String())}
	}

	var issuer issuerEntry
	if err := entry.DecodeJSON(&issuer); err != nil {
		return nil, errutil.InternalError{Err: fmt.Sprintf("unable to decode pki issuer with id %s: %v", issuerId.String(), err)}
	}

	return &issuer, nil
}

func writeIssuer(ctx context.Context, s logical.Storage, issuer *issuerEntry) error {
	issuerId := issuer.ID

	json, err := logical.StorageEntryJSON(issuerPrefix+issuerId.String(), issuer)
	if err != nil {
		return err
	}

	return s.Put(ctx, json)
}

func deleteIssuer(ctx context.Context, s logical.Storage, id issuerID) (bool, error) {
	wasDefault := false

	config, err := getIssuersConfig(ctx, s)
	if err != nil {
		return wasDefault, err
	}

	if config.DefaultIssuerId == id {
		wasDefault = true
		config.DefaultIssuerId = issuerID("")
		if err := setIssuersConfig(ctx, s, config); err != nil {
			return wasDefault, err
		}
	}

	return wasDefault, s.Delete(ctx, issuerPrefix+id.String())
}

func importIssuer(ctx context.Context, s logical.Storage, certValue string, issuerName string) (*issuerEntry, bool, error) {
	// importIssuers imports the specified PEM-format certificate (from
	// certValue) into the new PKI storage format. The first return field is a
	// reference to the new issuer; the second is whether or not the issuer
	// already existed during import (in which case, *issuer points to the
	// existing issuer reference and identifier); the last return field is
	// whether or not an error occurred.

	// Before we begin, we need to ensure the PEM formatted certificate looks
	// good. Restricting to "just" `CERTIFICATE` entries is a little
	// restrictive, as it could be a `X509 CERTIFICATE` entry or a custom
	// value wrapping an actual DER cert. So validating the contents of the
	// PEM header is out of the question (and validating the contents of the
	// PEM block is left to our GetCertificate call below).
	//
	// However, we should trim all leading and trailing spaces and add a
	// single new line. This allows callers to blindly concatenate PEM
	// blobs from the API and get roughly what they'd expect.
	//
	// Discussed further in #11960 and RFC 7468.
	certValue = strings.TrimSpace(certValue) + "\n"

	// Before we can import a known issuer, we first need to know if the issuer
	// exists in storage already. This means iterating through all known
	// issuers and comparing their private value against this value.
	knownIssuers, err := listIssuers(ctx, s)
	if err != nil {
		return nil, false, err
	}

	// Before we return below, we need to iterate over _all_ keys and see if
	// one of them a public key matching this certificate, and if so, update our
	// link accordingly. We fetch the list of keys up front, even may not need
	// it, to give ourselves a better chance of succeeding below.
	knownKeys, err := listKeys(ctx, s)
	if err != nil {
		return nil, false, err
	}

	for _, identifier := range knownIssuers {
		existingIssuer, err := fetchIssuerById(ctx, s, identifier)
		if err != nil {
			return nil, false, err
		}

		if existingIssuer.Certificate == certValue {
			// Here, we don't need to stitch together the key entries,
			// because the last run should've done that for us (or, when
			// importing a key).
			return existingIssuer, true, nil
		}
	}

	// Haven't found an issuer, so we've gotta create it and write it into
	// storage.
	var result issuerEntry
	result.ID = genIssuerId()
	result.Name = issuerName
	result.Certificate = certValue
	result.LeafNotAfterBehavior = certutil.ErrNotAfterBehavior

	// We shouldn't add CSRs or multiple certificates in this
	countCertificates := strings.Count(result.Certificate, "-BEGIN ")
	if countCertificates != 1 {
		return nil, false, fmt.Errorf("bad issuer: potentially multiple PEM blobs in one certificate storage entry:\n%v", result.Certificate)
	}

	// Extracting the certificate is necessary for two reasons: first, it lets
	// us fetch the serial number; second, for the public key comparison with
	// known keys.
	issuerCert, err := result.GetCertificate()
	if err != nil {
		return nil, false, err
	}

	// Ensure this certificate is a usable as a CA certificate.
	if !issuerCert.BasicConstraintsValid || !issuerCert.IsCA {
		return nil, false, errutil.UserError{Err: "Refusing to import non-CA certificate"}
	}

	result.SerialNumber = strings.TrimSpace(certutil.GetHexFormatted(issuerCert.SerialNumber.Bytes(), ":"))

	// Now, for each key, try and compute the issuer<->key link. We delay
	// writing issuer to storage as we won't need to update the key, only
	// the issuer.
	for _, identifier := range knownKeys {
		existingKey, err := fetchKeyById(ctx, s, identifier)
		if err != nil {
			return nil, false, err
		}

		// Fetch the signer for its Public() value.
		signer, err := existingKey.GetSigner()
		if err != nil {
			return nil, false, err
		}

		equal, err := certutil.ComparePublicKeysAndType(issuerCert.PublicKey, signer.Public())
		if err != nil {
			return nil, false, err
		}

		if equal {
			result.KeyID = existingKey.ID
			// Here, there's exactly one stored key with the same public key
			// as us, per guarantees in importKey; as we're importing an
			// issuer, there's no other keys or issuers we'd need to read or
			// update, so exit.
			break
		}
	}

	// Finally, rebuild the chains. In this process, because the provided
	// reference issuer is non-nil, we'll save this issuer to storage.
	if err := rebuildIssuersChains(ctx, s, &result); err != nil {
		return nil, false, err
	}

	// If there was no prior default value set and/or we had no known
	// issuers when we started, set this issuer as default.
	issuerDefaultSet, err := isDefaultIssuerSet(ctx, s)
	if err != nil {
		return nil, false, err
	}
	if len(knownIssuers) == 0 || !issuerDefaultSet {
		if err = updateDefaultIssuerId(ctx, s, result.ID); err != nil {
			return nil, false, err
		}
	}

	// All done; return our new key reference.
	return &result, false, nil
}

func setLocalCRLConfig(ctx context.Context, s logical.Storage, mapping *localCRLConfigEntry) error {
	json, err := logical.StorageEntryJSON(storageLocalCRLConfig, mapping)
	if err != nil {
		return err
	}

	return s.Put(ctx, json)
}

func getLocalCRLConfig(ctx context.Context, s logical.Storage) (*localCRLConfigEntry, error) {
	entry, err := s.Get(ctx, storageLocalCRLConfig)
	if err != nil {
		return nil, err
	}

	mapping := &localCRLConfigEntry{}
	if entry != nil {
		if err := entry.DecodeJSON(mapping); err != nil {
			return nil, errutil.InternalError{Err: fmt.Sprintf("unable to decode cluster-local CRL configuration: %v", err)}
		}
	}

	if len(mapping.IssuerIDCRLMap) == 0 {
		mapping.IssuerIDCRLMap = make(map[issuerID]crlID)
	}

	if len(mapping.CRLNumberMap) == 0 {
		mapping.CRLNumberMap = make(map[crlID]int64)
	}

	return mapping, nil
}

func setKeysConfig(ctx context.Context, s logical.Storage, config *keyConfigEntry) error {
	json, err := logical.StorageEntryJSON(storageKeyConfig, config)
	if err != nil {
		return err
	}

	return s.Put(ctx, json)
}

func getKeysConfig(ctx context.Context, s logical.Storage) (*keyConfigEntry, error) {
	entry, err := s.Get(ctx, storageKeyConfig)
	if err != nil {
		return nil, err
	}

	keyConfig := &keyConfigEntry{}
	if entry != nil {
		if err := entry.DecodeJSON(keyConfig); err != nil {
			return nil, errutil.InternalError{Err: fmt.Sprintf("unable to decode key configuration: %v", err)}
		}
	}

	return keyConfig, nil
}

func setIssuersConfig(ctx context.Context, s logical.Storage, config *issuerConfigEntry) error {
	json, err := logical.StorageEntryJSON(storageIssuerConfig, config)
	if err != nil {
		return err
	}

	return s.Put(ctx, json)
}

func getIssuersConfig(ctx context.Context, s logical.Storage) (*issuerConfigEntry, error) {
	entry, err := s.Get(ctx, storageIssuerConfig)
	if err != nil {
		return nil, err
	}

	issuerConfig := &issuerConfigEntry{}
	if entry != nil {
		if err := entry.DecodeJSON(issuerConfig); err != nil {
			return nil, errutil.InternalError{Err: fmt.Sprintf("unable to decode issuer configuration: %v", err)}
		}
	}

	return issuerConfig, nil
}

func resolveIssuerReference(ctx context.Context, s logical.Storage, reference string) (issuerID, error) {
	if reference == defaultRef {
		// Handle fetching the default issuer.
		config, err := getIssuersConfig(ctx, s)
		if err != nil {
			return issuerID("config-error"), err
		}
		if len(config.DefaultIssuerId) == 0 {
			return IssuerRefNotFound, fmt.Errorf("no default issuer currently configured")
		}

		return config.DefaultIssuerId, nil
	}

	issuers, err := listIssuers(ctx, s)
	if err != nil {
		return issuerID("list-error"), err
	}

	// Cheaper to list issuers and check if an id is a match...
	for _, issuerId := range issuers {
		if issuerId == issuerID(reference) {
			return issuerId, nil
		}
	}

	// ... than to pull all issuers from storage.
	for _, issuerId := range issuers {
		issuer, err := fetchIssuerById(ctx, s, issuerId)
		if err != nil {
			return issuerID("issuer-read"), err
		}

		if issuer.Name == reference {
			return issuer.ID, nil
		}
	}

	// Otherwise, we must not have found the issuer.
	return IssuerRefNotFound, errutil.UserError{Err: fmt.Sprintf("unable to find PKI issuer for reference: %v", reference)}
}

func resolveIssuerCRLPath(ctx context.Context, b *backend, s logical.Storage, reference string) (string, error) {
	if b.useLegacyBundleCaStorage() {
		return "crl", nil
	}

	issuer, err := resolveIssuerReference(ctx, s, reference)
	if err != nil {
		return "crl", err
	}

	crlConfig, err := getLocalCRLConfig(ctx, s)
	if err != nil {
		return "crl", err
	}

	if crlId, ok := crlConfig.IssuerIDCRLMap[issuer]; ok && len(crlId) > 0 {
		return fmt.Sprintf("crls/%v", crlId), nil
	}

	return "crl", fmt.Errorf("unable to find CRL for issuer: id:%v/ref:%v", issuer, reference)
}

// Builds a certutil.CertBundle from the specified issuer identifier,
// optionally loading the key or not.
func fetchCertBundleByIssuerId(ctx context.Context, s logical.Storage, id issuerID, loadKey bool) (*issuerEntry, *certutil.CertBundle, error) {
	issuer, err := fetchIssuerById(ctx, s, id)
	if err != nil {
		return nil, nil, err
	}

	var bundle certutil.CertBundle
	bundle.Certificate = issuer.Certificate
	bundle.CAChain = issuer.CAChain
	bundle.SerialNumber = issuer.SerialNumber

	// Fetch the key if it exists. Sometimes we don't need the key immediately.
	if loadKey && issuer.KeyID != keyID("") {
		key, err := fetchKeyById(ctx, s, issuer.KeyID)
		if err != nil {
			return nil, nil, err
		}

		bundle.PrivateKeyType = key.PrivateKeyType
		bundle.PrivateKey = key.PrivateKey
	}

	return issuer, &bundle, nil
}

func writeCaBundle(ctx context.Context, s logical.Storage, caBundle *certutil.CertBundle, issuerName string, keyName string) (*issuerEntry, *keyEntry, error) {
	myKey, _, err := importKey(ctx, s, caBundle.PrivateKey, keyName)
	if err != nil {
		return nil, nil, err
	}

	myIssuer, _, err := importIssuer(ctx, s, caBundle.Certificate, issuerName)
	if err != nil {
		return nil, nil, err
	}

	for _, cert := range caBundle.CAChain {
		if _, _, err = importIssuer(ctx, s, cert, ""); err != nil {
			return nil, nil, err
		}
	}

	return myIssuer, myKey, nil
}

func genIssuerId() issuerID {
	return issuerID(genUuid())
}

func genKeyId() keyID {
	return keyID(genUuid())
}

func genCRLId() crlID {
	return crlID(genUuid())
}

func genUuid() string {
	aUuid, err := uuid.GenerateUUID()
	if err != nil {
		panic(err)
	}
	return aUuid
}

func isKeyInUse(keyId string, ctx context.Context, s logical.Storage) (inUse bool, issuerId string, err error) {
	knownIssuers, err := listIssuers(ctx, s)
	if err != nil {
		return true, "", err
	}

	for _, issuerId := range knownIssuers {
		issuerEntry, err := fetchIssuerById(ctx, s, issuerId)
		if err != nil {
			return true, issuerId.String(), errutil.InternalError{Err: fmt.Sprintf("unable to fetch pki issuer: %v", err)}
		}
		if issuerEntry == nil {
			return true, issuerId.String(), errutil.InternalError{Err: fmt.Sprintf("Issuer listed: %s does not exist", issuerId.String())}
		}
		if issuerEntry.KeyID.String() == keyId {
			return true, issuerId.String(), nil
		}
	}

	return false, "", nil
}
