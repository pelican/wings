package router

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/filesystem"
)

// Structs needed to respond with the matched files and all their info
type customFileInfo struct {
	os.FileInfo
	newName string
}

func (cfi customFileInfo) Name() string {
	return cfi.newName // Return the custom name (i.e., with the directory prefix)
}

// Helper function to append matched entries
func appendMatchedEntry(matchedEntries *[]filesystem.Stat, fileInfo os.FileInfo, fullPath string, fileType string) {
	*matchedEntries = append(*matchedEntries, filesystem.Stat{
		FileInfo: customFileInfo{
			FileInfo: fileInfo,
			newName:  fullPath,
		},
		Mimetype: fileType,
	})
}

// todo make this config value work as now it cause a panic
//var blacklist = config.Get().SearchRecursion.BlacklistedDirs

var blacklist = []string{"node_modules", ".wine", ".git", "appcache", "depotcache", "vendor"}

// Helper function to check if a directory name is in the blacklist
func isBlacklisted(dirName string) bool {
	for _, blacklisted := range blacklist {
		if strings.EqualFold(dirName, strings.ToLower(blacklisted)) {
			return true
		}
	}
	return false
}

// Recursive function to search through directories
func searchDirectory(s *server.Server, dir string, patternLower string, depth int, matchedEntries *[]filesystem.Stat, matchedDirectories *[]string, c *gin.Context) {
	if depth > config.Get().SearchRecursion.MaxRecursionDepth {
		return // Stop recursion if depth exceeds
	}

	stats, err := s.Filesystem().ListDirectory(dir)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "Directory not found"})
		return
	}

	for _, fileInfo := range stats {
		fileName := fileInfo.Name()
		fileType := fileInfo.Mimetype
		fileNameLower := strings.ToLower(fileName)
		fullPath := filepath.Join(dir, fileName)

		// Store directories separately
		if fileType == "inode/directory" {
			if isBlacklisted(fileNameLower) {
				continue // Skip blacklisted directories
			}
			*matchedDirectories = append(*matchedDirectories, fullPath)

			// Recursive search in the matched directory
			searchDirectory(s, fullPath, patternLower, depth+1, matchedEntries, matchedDirectories, c)
		}

		// Wildcard or exact matching logic
		if strings.ContainsAny(patternLower, "*?") {
			if match, _ := filepath.Match(patternLower, fileNameLower); match {
				appendMatchedEntry(matchedEntries, fileInfo, fullPath, fileType)
			}
		} else {
			// Check for substring matches (case-insensitive)
			if strings.Contains(fileNameLower, patternLower) {
				appendMatchedEntry(matchedEntries, fileInfo, fullPath, fileType)
			} else {
				// Extension matching logic
				ext := filepath.Ext(fileNameLower)
				if strings.HasPrefix(patternLower, ".") || !strings.Contains(patternLower, ".") {
					// Match extension without dot
					if strings.TrimPrefix(ext, ".") == strings.TrimPrefix(patternLower, ".") {
						appendMatchedEntry(matchedEntries, fileInfo, fullPath, fileType)
					}
				} else if fileNameLower == patternLower { // Full name match
					appendMatchedEntry(matchedEntries, fileInfo, fullPath, fileType)
				}
			}
		}
	}
}

func getFilesBySearch(c *gin.Context) {
	s := middleware.ExtractServer(c)
	dir := strings.TrimSuffix(c.Query("directory"), "/")
	pattern := c.Query("pattern")

	// Convert the pattern to lowercase for case-insensitive comparison
	patternLower := strings.ToLower(pattern)

	// Check if the pattern length is at least 3 characters
	if len(pattern) < 3 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Pattern must be at least 3 characters long"})
		return
	}

	// Prepare slices to store matched stats and directories
	matchedEntries := []filesystem.Stat{}
	matchedDirectories := []string{}

	// Start the search from the initial directory
	searchDirectory(s, dir, patternLower, 0, &matchedEntries, &matchedDirectories, c)

	// Return all matched files with their stats and the name now included the directory
	c.JSON(http.StatusOK, matchedEntries)

}
