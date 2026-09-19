// SPDX-License-Identifier: AGPL-3.0-only
package server

import "encoding/json"

func validFolders(folders []string) bool {
	if len(folders) > 32 {
		return false
	}
	seen := map[string]bool{}
	for _, folder := range folders {
		if folder == "" || !validFolder(folder) || seen[folder] {
			return false
		}
		seen[folder] = true
	}
	return true
}

func metadataFolders(folder string, folders []string) []string {
	if folders != nil {
		return folders
	}
	if folder != "" && validFolder(folder) {
		return []string{folder}
	}
	return []string{}
}

// An old client's primary-folder change replaces only that membership. Explicit
// modern arrays are authoritative; absence preserves all current memberships.
func canonicalBookFolders(value, previous json.RawMessage) json.RawMessage {
	var incoming, old map[string]json.RawMessage
	_ = json.Unmarshal(value, &incoming)
	_ = json.Unmarshal(previous, &old)
	var folder, oldFolder string
	var folders, oldFolders []string
	_ = json.Unmarshal(incoming["folder"], &folder)
	_ = json.Unmarshal(old["folder"], &oldFolder)
	_ = json.Unmarshal(old["folders"], &oldFolders)
	if _, present := incoming["folders"]; present {
		_ = json.Unmarshal(incoming["folders"], &folders)
	} else {
		folders = metadataFolders(oldFolder, oldFolders)
		primary := ""
		if len(folders) > 0 {
			primary = folders[0]
		}
		if _, present := incoming["folder"]; present && folder != primary {
			if len(folders) > 0 {
				folders = folders[1:]
			}
			next := []string{}
			if folder != "" {
				next = append(next, folder)
			}
			for _, f := range folders {
				if f != folder {
					next = append(next, f)
				}
			}
			folders = next
		}
	}
	if folders == nil {
		folders = []string{}
	}
	folder = ""
	if len(folders) > 0 {
		folder = folders[0]
	}
	incoming["folder"], _ = json.Marshal(folder)
	incoming["folders"], _ = json.Marshal(folders)
	if incoming["format"] == nil && old["format"] != nil {
		incoming["format"] = old["format"]
	}
	out, _ := json.Marshal(incoming)
	return out
}
