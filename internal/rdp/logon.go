package rdp

// logonInfo reports the identity used to authenticate, without replacing the
// client's domain or username with an invented local Windows account. The
// client decides whether to persist its credentials after this notification.
func logonInfo(domain, username string, sessionID uint32, longCredentials bool) []byte {
	domainBytes := join(utf16Bytes(domain), u16(0))
	userBytes := join(utf16Bytes(username), u16(0))
	info := make([]byte, 576)
	if longCredentials {
		// TS_LOGON_INFO_VERSION_2: fixed header and padding, then two strings.
		le.PutUint16(info, 1) // SAVE_SESSION_PDU_VERSION_ONE
		le.PutUint32(info[2:], uint32(len(info)))
		le.PutUint32(info[6:], sessionID)
		le.PutUint32(info[10:], uint32(len(domainBytes)))
		le.PutUint32(info[14:], uint32(len(userBytes)))
		return shareData(38, join(u32(1), info, domainBytes, userBytes)) // INFOTYPE_LOGON_LONG
	}
	if len(domainBytes) > 52 || len(userBytes) > 512 {
		// A client without LONG_CREDENTIALS_SUPPORTED cannot receive this
		// identity in full. Notify success without truncating a saved account.
		return shareData(38, join(u32(2), info)) // INFOTYPE_LOGON_PLAINNOTIFY
	}
	le.PutUint32(info, uint32(len(domainBytes)))
	copy(info[4:56], domainBytes)
	le.PutUint32(info[56:], uint32(len(userBytes)))
	copy(info[60:572], userBytes)
	le.PutUint32(info[572:], sessionID)
	return shareData(38, join(u32(0), info)) // INFOTYPE_LOGON
}

func (c *client) sendLogonInfo() error {
	if c.logonNotified || c.logonUser == "" {
		return nil
	}
	if err := c.sendMCS(ioChannel, logonInfo(c.logonDomain, c.logonUser, c.logonSessionID, c.longCredentials)); err != nil {
		return err
	}
	c.logonNotified = true
	return nil
}
