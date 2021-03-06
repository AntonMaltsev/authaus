package authaus

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	/* Number of characters from the set [a-zA-Z0-9] = 62. 62^30 = 6 x 10^53, which is 178 bits of entropy.
	Assume there will be 1 million valid tokens. That removes 20 bits of entropy, leaving 158 bits.
	Divide 158 by 2 and we have a security level of 79 bits. If an attacker can try 100000 tokens per
	second, then it would take 2 * 10^11 years to find a random good token.
	*/
	sessionTokenLength = 30

	defaultSessionExpirySeconds = 30 * 24 * 3600
)

var (
	// NOTE: These 'base' error strings may not be prefixes of each other,
	// otherwise it violates our NewError() concept, which ensures that
	// any Authaus error starts with one of these *unique* prefixes
	ErrConnect                = errors.New("Connect failed")
	ErrUnsupported            = errors.New("Unsupported operation")
	ErrIdentityAuthNotFound   = errors.New("Identity authorization not found")
	ErrIdentityPermitNotFound = errors.New("Identity permit not found")
	ErrIdentityEmpty          = errors.New("Identity may not be empty")
	ErrIdentityExists         = errors.New("Identity already exists")
	ErrInvalidPassword        = errors.New("Invalid password")
	ErrInvalidSessionToken    = errors.New("Invalid session token")
)

// Use this whenever you return an Authaus error. We rely upon the prefix
// of the error string to identify the broad category of the error.
func NewError(base error, detail string) error {
	return errors.New(base.Error() + ": " + detail)
}

// A Permit is an opaque binary string that encodes domain-specific roles.
// This could be a string of bits with special meanings, or a blob of JSON, etc.
type Permit struct {
	Roles []byte
}

func (x *Permit) Clone() *Permit {
	cpy := &Permit{}
	cpy.Roles = make([]byte, len(x.Roles))
	copy(cpy.Roles, x.Roles)
	return cpy
}

func (x *Permit) Serialize() string {
	return base64.StdEncoding.EncodeToString(x.Roles)
}

func (x *Permit) Deserialize(encoded string) error {
	*x = Permit{}
	if roles, e := base64.StdEncoding.DecodeString(encoded); e == nil {
		x.Roles = roles
		return nil
	} else {
		return e
	}
}

func (a *Permit) Equals(b *Permit) bool {
	return bytes.Equal(a.Roles, b.Roles)
}

/*
Token is the result of a successful authentication request. It contains
everything that we know about this authentication event, which includes
the identity that performed the request, when this token expires, and
the permit belonging to this identity.
*/
type Token struct {
	Identity string
	Expires  time.Time
	Permit   Permit
}

// Transform an identity into its canonical form. What this means is that any two identities
// are considered equal if their canonical forms are equal. This is simply a lower-casing
// of the identity, so that "bob@enterprise.com" is equal to "Bob@enterprise.com".
func CanonicalizeIdentity(identity string) string {
	return strings.ToLower(identity)
}

// Returns a random string of 'nchars' characters, sampled uniformly from the given corpus of characters.
func randomString(nchars int, corpus string) string {
	rbytes := make([]byte, nchars)
	rstring := make([]byte, nchars)
	rand.Read(rbytes)
	for i := 0; i < nchars; i++ {
		rstring[i] = corpus[rbytes[i]%byte(len(corpus))]
	}
	return string(rstring)
}

func generateSessionKey() string {
	// It is important not to have any unusual characters in here, especially an equals sign. Old versions of Tomcat
	// will parse such a cookie incorrectly (imagine Cookie: magic=abracadabra=)
	return randomString(sessionTokenLength, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

type CentralStats struct {
	InvalidSessionKeys uint64
	ExpiredSessionKeys uint64
	InvalidPasswords   uint64
	EmptyIdentities    uint64
	GoodOnceOffAuth    uint64
	GoodLogin          uint64
	Logout             uint64
}

func isPowerOf2(x uint64) bool {
	return 0 == x&(x-1)
}

func (x *CentralStats) IncrementAndLog(name string, val *uint64, logger *log.Logger) {
	n := atomic.AddUint64(val, 1)
	if isPowerOf2(n) || (n&255) == 0 {
		logger.Printf("%v %v", n, name)
	}
}

func (x *CentralStats) IncrementInvalidSessionKey(logger *log.Logger) {
	x.IncrementAndLog("invalid session keys", &x.InvalidSessionKeys, logger)
}

func (x *CentralStats) IncrementExpiredSessionKey(logger *log.Logger) {
	x.IncrementAndLog("expired session keys", &x.ExpiredSessionKeys, logger)
}

func (x *CentralStats) IncrementInvalidPasswords(logger *log.Logger) {
	x.IncrementAndLog("invalid passwords", &x.InvalidPasswords, logger)
}

func (x *CentralStats) IncrementEmptyIdentities(logger *log.Logger) {
	x.IncrementAndLog("empty identities", &x.EmptyIdentities, logger)
}

func (x *CentralStats) IncrementGoodOnceOffAuth(logger *log.Logger) {
	x.IncrementAndLog("good once-off auth", &x.GoodOnceOffAuth, logger)
}

func (x *CentralStats) IncrementGoodLogin(logger *log.Logger) {
	x.IncrementAndLog("good login", &x.GoodLogin, logger)
}

func (x *CentralStats) IncrementLogout(logger *log.Logger) {
	x.IncrementAndLog("logout", &x.Logout, logger)
}

/*
For lack of a better name, this is the single hub of authentication that you interact with.
All public methods of Central are callable from multiple threads.
*/
type Central struct {
	authenticator          Authenticator
	permitDB               PermitDB
	sessionDB              SessionDB
	roleGroupDB            RoleGroupDB
	logFile                *os.File
	Log                    *log.Logger
	Stats                  CentralStats
	MaxActiveSessions      int32
	NewSessionExpiresAfter time.Duration
}

// Create a new Central object from the specified pieces.
// roleGroupDB may be nil
func NewCentral(logger *log.Logger, authenticator Authenticator, permitDB PermitDB, sessionDB SessionDB, roleGroupDB RoleGroupDB) *Central {
	c := &Central{}
	c.authenticator = &sanitizingAuthenticator{
		backend: authenticator,
	}
	c.permitDB = permitDB
	c.sessionDB = newCachedSessionDB(sessionDB)
	if roleGroupDB != nil {
		c.roleGroupDB = NewCachedRoleGroupDB(roleGroupDB)
	}
	c.MaxActiveSessions = 0
	c.NewSessionExpiresAfter = time.Duration(defaultSessionExpirySeconds) * time.Second
	c.Log = logger
	c.Log.Printf("Authaus successfully started up\n")
	return c
}

// Create a new 'Central' object from a Config.
func NewCentralFromConfig(config *Config) (central *Central, err error) {
	var logfile *os.File
	if config.Log.Filename != "" {
		if config.Log.Filename == "stdout" {
			logfile = os.Stdout
		} else if config.Log.Filename == "stderr" {
			logfile = os.Stderr
		} else {
			if logfile, err = os.OpenFile(config.Log.Filename, os.O_APPEND|os.O_CREATE, 0660); err != nil {
				return nil, errors.New(fmt.Sprintf("Error opening log file '%v': %v", config.Log.Filename, err))
			}
		}
	} else {
		logfile = os.Stdout
	}

	logger := log.New(logfile, "", log.Ldate|log.Ltime|log.Lmicroseconds)

	var auth Authenticator
	var permitDB PermitDB
	var sessionDB SessionDB
	var roleGroupDB RoleGroupDB

	defer func() {
		if ePanic := recover(); ePanic != nil {
			if auth != nil {
				auth.Close()
			}
			if permitDB != nil {
				permitDB.Close()
			}
			if sessionDB != nil {
				sessionDB.Close()
			}
			if roleGroupDB != nil {
				roleGroupDB.Close()
			}
			logger.Printf("Error initializing: %v\n", ePanic)
			err = ePanic.(error)
		}
	}()

	if config.SessionDB.MaxActiveSessions < 0 || config.SessionDB.MaxActiveSessions > 1 {
		panic(errors.New("MaxActiveSessions must be 0 or 1"))
	}

	if config.SessionDB.SessionExpirySeconds < 0 {
		panic(errors.New("SessionExpirySeconds must be 0 or more"))
	}

	if auth, err = createAuthenticator(&config.Authenticator); err != nil {
		panic(err)
	}

	if permitDB, err = NewPermitDB_SQL(&config.PermitDB.DB); err != nil {
		panic(errors.New(fmt.Sprintf("Error connecting to PermitDB: %v", err)))
	}

	if sessionDB, err = NewSessionDB_SQL(&config.SessionDB.DB); err != nil {
		panic(errors.New(fmt.Sprintf("Error connecting to SessionDB: %v", err)))
	}

	if config.RoleGroupDB.DB.Driver != "" {
		if roleGroupDB, err = NewRoleGroupDB_SQL(&config.RoleGroupDB.DB); err != nil {
			panic(errors.New(fmt.Sprintf("Error connecting to RoleGroupDB: %v", err)))
		}
	}

	c := NewCentral(logger, auth, permitDB, sessionDB, roleGroupDB)
	c.logFile = logfile
	c.MaxActiveSessions = config.SessionDB.MaxActiveSessions
	if config.SessionDB.SessionExpirySeconds != 0 {
		c.NewSessionExpiresAfter = time.Duration(config.SessionDB.SessionExpirySeconds) * time.Second
	}
	return c, nil
}

func createAuthenticator(config *ConfigAuthenticator) (Authenticator, error) {
	var err error
	var auth Authenticator
	switch config.Type {
	case "ldap":
		ldapMode, legalLdapMode := configLdapNameToMode[config.Encryption]
		//ldapAddress := config.Authenticator.LdapHost
		//if config.Authenticator.LdapPort != 0 {
		//	ldapAddress += ":" + strconv.Itoa(int(config.Authenticator.LdapPort))
		//}
		if !legalLdapMode {
			return nil, errors.New(fmt.Sprintf("Unknown ldap mode %v. Recognized modes are TLS, SSL, and empty for unencrypted", config.Encryption))
		}
		//if auth, err = NewAuthenticator_LDAP(ldapMode, "tcp", ldapAddress); err != nil {
		if auth, err = NewAuthenticator_LDAP(ldapMode, config.LdapHost, uint16(config.LdapPort)); err != nil {
			return nil, errors.New(fmt.Sprintf("Error creating LDAP Authenticator: %v", err))
		}
		return auth, nil
	case "db":
		if auth, err = NewAuthenticationDB_SQL(&config.DB); err != nil {
			return nil, errors.New(fmt.Sprintf("Unable to connect to AuthenticationDB: %v", err))
		}
		return auth, nil
	case "dummy":
		return newDummyAuthenticator(), nil
	default:
		return nil, errors.New("Unrecognized Authenticator type '" + config.Type + "'")
	}
}

// Set the size of the in-memory session cache
func (x *Central) SetSessionCacheSize(maxSessions int) {
	x.sessionDB.(*cachedSessionDB).MaxCachedSessions = maxSessions
}

// Pass in a session key that was generated with a call to Login(), and get back a token.
// A session key is typically a cookie.
func (x *Central) GetTokenFromSession(sessionkey string) (*Token, error) {
	if token, err := x.sessionDB.Read(sessionkey); err != nil {
		x.Stats.IncrementInvalidSessionKey(x.Log)
		return token, err
	} else {
		if time.Now().UnixNano() > token.Expires.UnixNano() {
			// DB has not yet expired token. It's OK for the DB to be a bit lazy in its cleanup.
			x.Stats.IncrementExpiredSessionKey(x.Log)
			return nil, ErrInvalidSessionToken
		} else {
			return token, err
		}
	}
}

// Perform a once-off authentication
func (x *Central) GetTokenFromIdentityPassword(identity, password string) (*Token, error) {
	// Treat empty identity specially, since this is a very common condition, and
	// tends to flood the logs.
	// Some day we may realize that it is better to emit the IP addresses here, even
	// for empty identity authorization requests.
	if identity == "" {
		x.Stats.IncrementEmptyIdentities(x.Log)
		return nil, ErrIdentityEmpty
	}
	if eAuth := x.authenticator.Authenticate(identity, password); eAuth == nil {
		if permit, ePermit := x.permitDB.GetPermit(identity); ePermit == nil {
			t := &Token{}
			t.Expires = veryFarFuture
			t.Identity = identity
			t.Permit = *permit
			x.Stats.IncrementGoodOnceOffAuth(x.Log)
			x.Log.Printf("Once-off auth successful (%v)", identity)
			return t, nil
		} else {
			x.Log.Printf("Once-off auth GetPermit failed (%v) (%v)", identity, ePermit)
			return nil, ePermit
		}
	} else {
		x.Stats.IncrementInvalidPasswords(x.Log)
		x.Log.Printf("Once-off auth Authentication failed (%v) (%v)", identity, eAuth)
		return nil, eAuth
	}
}

// Create a new session. Returns a session key, which can be used in future to retrieve the token.
// The internal session expiry is controlled with the member NewSessionExpiresAfter.
// The session key is typically sent to the client as a cookie.
func (x *Central) Login(identity, password string) (sessionkey string, token *Token, e error) {
	token = &Token{}
	token.Identity = identity
	if e = x.authenticator.Authenticate(identity, password); e == nil {
		x.Log.Printf("Login authentication success (%v)", identity)
		var permit *Permit
		if permit, e = x.permitDB.GetPermit(identity); e == nil {
			if x.MaxActiveSessions != 0 {
				if e = x.sessionDB.InvalidateSessionsForIdentity(identity); e != nil {
					x.Log.Printf("Invalidate sessions for identity (%v) failed when enforcing MaxActiveSessions (%v)", identity, e)
					return "", nil, e
				}
			}
			token.Expires = time.Now().Add(x.NewSessionExpiresAfter)
			token.Permit = *permit
			sessionkey = generateSessionKey()
			if e = x.sessionDB.Write(sessionkey, token); e == nil {
				x.Stats.IncrementGoodLogin(x.Log)
				x.Log.Printf("Login successful (%v)", identity)
				return
			}
		} else {
			x.Log.Printf("Login GetPermit failed (%v) (%v)", identity, e)
		}
	} else {
		x.Stats.IncrementInvalidPasswords(x.Log)
		x.Log.Printf("Login Authentication failed (%v) (%v)", identity, e)
	}
	sessionkey = ""
	token = nil
	return
}

// Logout, which erases the session key
func (x *Central) Logout(sessionkey string) error {
	x.Stats.IncrementLogout(x.Log)
	return x.sessionDB.Delete(sessionkey)
}

// Invalidate all sessions for a particular identity
func (x *Central) InvalidateSessionsForIdentity(identity string) error {
	return x.sessionDB.InvalidateSessionsForIdentity(identity)
}

// Retrieve a Permit.
func (x *Central) GetPermit(identity string) (*Permit, error) {
	return x.permitDB.GetPermit(identity)
}

// Retrieve all Permits.
func (x *Central) GetPermits() (map[string]*Permit, error) {
	return x.permitDB.GetPermits()
}

// Change a Permit.
func (x *Central) SetPermit(identity string, permit *Permit) error {
	if err := x.permitDB.SetPermit(identity, permit); err != nil {
		x.Log.Printf("SetPermit failed (%v) (%v)", identity, err)
		return err
	}
	x.Log.Printf("SetPermit successful (%v)", identity)
	return x.sessionDB.PermitChanged(identity, permit)
}

// Change a Password. This invalidates all sessions for this identity.
func (x *Central) SetPassword(identity, password string) error {
	if err := x.authenticator.SetPassword(identity, password); err != nil {
		x.Log.Printf("SetPassword failed (%v) (%v)", identity, password)
		return err
	}
	x.Log.Printf("SetPassword successful (%v)", identity)
	return x.sessionDB.InvalidateSessionsForIdentity(identity)
}

// Create an identity in the Authenticator.
// For the equivalent operation in the PermitDB, simply call SetPermit()
func (x *Central) CreateAuthenticatorIdentity(identity, password string) error {
	e := x.authenticator.CreateIdentity(identity, password)
	if e == nil {
		x.Log.Printf("CreateAuthenticatorIdentity successful (%v)", identity)
	} else {
		x.Log.Printf("CreateAuthenticatorIdentity failed (%v) (%v)", identity, e)
	}
	return e
}

// Retrieve all identities known to the Authenticator.
func (x *Central) GetAuthenticatorIdentities() ([]string, error) {
	return x.authenticator.GetIdentities()
}

// Retrieve the Role Group Database (which may be nil)
func (x *Central) GetRoleGroupDB() RoleGroupDB {
	return x.roleGroupDB
}

func (x *Central) Close() {
	if x.Log != nil {
		x.Log.Printf("Authaus shutting down\n")
		x.Log = nil
	}
	if x.logFile != nil {
		x.logFile.Close()
		x.logFile = nil
	}
	if x.authenticator != nil {
		x.authenticator.Close()
		x.authenticator = nil
	}
	if x.permitDB != nil {
		x.permitDB.Close()
		x.permitDB = nil
	}
	if x.sessionDB != nil {
		x.sessionDB.Close()
		x.sessionDB = nil
	}
	if x.roleGroupDB != nil {
		x.roleGroupDB.Close()
		x.roleGroupDB = nil
	}
}

func (x *Central) debugEnableSessionDB(enable bool) {
	// Used for testing the session cache
	x.sessionDB.(*cachedSessionDB).enableDB = enable
}
