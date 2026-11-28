package main

import "os"

// ttyWidth: not asked on Windows; the table is fitted to 100 columns.
func ttyWidth(*os.File) int { return 0 }
